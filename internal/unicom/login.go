package unicom

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// 网关协议登录（unicomvue 公共网关 networkapi.2t.hk）：
// 联通官方发码/登录接口已被腾讯滑块风控覆盖（纯 HTTP 直连被 WAF 拦截，实测 302），
// 登录走网关三步：send（发码）→ validate（前端腾讯滑块票据校验）→ login（短信验证码登录）。
// 滑块在面板前端弹出（TJCaptcha.js，appid 见 CaptchaAppID），票据经 SubmitCaptcha 注入本会话；
// 登录成功拿 ecs_token（查询 Cookie）与 onlin_token（token_online 会话维持），
// 此后查询与会话维持均直连联通官方接口 m.client.10010.com，不再经过网关。

const (
	// 整会话上限：覆盖滑块 + 短信到达 + 输入验证码
	gwSessionTimeout = 10 * time.Minute
	gwRequestTimeout = 30 * time.Second
)

// unicomSession 联通网关登录会话（无浏览器），
// 实现 carrier.LoginSession、carrier.CaptchaSession 与 carrier.LoginStateSaver。
type unicomSession struct {
	phone string
	log   *loggerx.Logger

	appID    string // 联通 APP 身份（登录与查询自愈共用）
	deviceID string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	codeCh chan string
	capCh  chan [2]string // {ticket, randstr}

	mu        sync.Mutex
	stage     string
	msg       string
	submitted bool
	capBusy   bool // 滑块校验进行中
	state     *LoginState
}

// StartLogin 启动联通验证码登录会话：立即发码，被风控时进入滑块等待（前端弹出）
func StartLogin(phone, dataDir string, headless bool, log *loggerx.Logger) (*unicomSession, error) {
	if !store.ValidPhone(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	_ = dataDir // 网关协议无需浏览器登录态目录（保留参数兼容 carrier.LoginParams）
	_ = headless

	ctx, cancel := context.WithTimeout(context.Background(), gwSessionTimeout)
	s := &unicomSession{
		phone:    phone,
		log:      log,
		appID:    generateAppID(),
		deviceID: generateDeviceID(),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		codeCh:   make(chan string, 1),
		capCh:    make(chan [2]string, 1),
	}
	go s.run()
	return s, nil
}

func (s *unicomSession) run() {
	defer close(s.done)
	defer s.cancel()

	s.setStage(carrier.StageCodeSending, "正在发送短信验证码...")

	// 初始发码（无滑块票据直接发，被风控时网关返回 need_captcha）
	mobile, err := s.doSend("")
	if err != nil {
		s.setStage(carrier.StageError, err.Error())
		return
	}
	if mobile == "" {
		// send 成功（或已发出），等待用户输入验证码
		s.setStage(carrier.StageWaitingCode, "验证码已发送，请输入收到的短信验证码")
	}

	for {
		select {
		case <-s.ctx.Done():
			s.setStage(carrier.StageClosed, "已取消或超时")
			return
		case pair := <-s.capCh:
			// 前端滑块完成：提交票据校验 → 携 resultToken 重发验证码
			ticket, randstr := pair[0], pair[1]
			s.setStage(carrier.StageCodeSending, "安全验证通过，正在发送短信验证码...")
			s.log.Info("[%s] 提交滑块票据校验", s.phone)
			ctx, cancel := context.WithTimeout(s.ctx, gwRequestTimeout)
			v, verr := gwValidate(ctx, ticket, randstr, mobile, s.phone, s.appID, s.deviceID)
			cancel()
			s.mu.Lock()
			s.capBusy = false
			s.mu.Unlock()
			if verr != nil {
				s.setStage(carrier.StageNeedCaptcha, "安全验证请求失败（"+verr.Error()+"），请重新完成滑块验证")
				continue
			}
			if v.Status != "success" || v.ResultToken == "" {
				msg := v.Msg
				if msg == "" {
					msg = "安全验证未通过"
				}
				s.setStage(carrier.StageNeedCaptcha, msg+"，请重新完成滑块验证")
				continue
			}
			mobile2, serr := s.doSend(v.ResultToken)
			if serr != nil {
				s.setStage(carrier.StageSendFailed, "验证码发送失败："+serr.Error())
				return
			}
			_ = mobile2
			s.setStage(carrier.StageWaitingCode, "验证码已发送，请输入收到的短信验证码")
		case code := <-s.codeCh:
			if s.submitted {
				continue
			}
			s.mu.Lock()
			s.submitted = true
			s.mu.Unlock()
			s.setStage(carrier.StageSubmitting, "")
			s.log.Info("[%s] 提交短信验证码登录", s.phone)
			ctx, cancel := context.WithTimeout(s.ctx, gwRequestTimeout)
			g, gerr := gwLogin(ctx, s.phone, code, s.appID, s.deviceID)
			cancel()
			if gerr != nil {
				s.setStage(carrier.StageError, "登录请求失败: "+gerr.Error())
				return
			}
			if g.Status != "success" || g.EcsToken == "" {
				msg := g.Msg
				if msg == "" {
					msg = "登录失败"
				}
				s.mu.Lock()
				s.submitted = false
				s.mu.Unlock()
				s.setStage(carrier.StageWaitingCode, "登录失败："+msg+"，请重新输入验证码（若验证码已失效请取消后重新获取）")
				continue
			}
			s.mu.Lock()
			s.state = &LoginState{
				AppID:       s.appID,
				TokenOnline: g.OnlinToken,
				Cookie:      "ecs_token=" + g.EcsToken,
			}
			s.mu.Unlock()
			s.setStage(carrier.StageSuccess, "")
			s.log.Info("[%s] 网关登录成功，已获取查询 Cookie", s.phone)
			return
		}
	}
}

// doSend 调网关发码：
//   - 成功：返回 ""（进入等待验证码）
//   - 被风控：置 need_captcha 阶段，返回 mobile 字段（validate 时需回传）
//   - 失败：返回 error（阶段由调用方决定）
func (s *unicomSession) doSend(resultToken string) (mobile string, err error) {
	ctx, cancel := context.WithTimeout(s.ctx, gwRequestTimeout)
	g, gerr := gwSend(ctx, s.phone, s.appID, s.deviceID, resultToken)
	cancel()
	if gerr != nil {
		return "", gerr
	}
	switch g.Status {
	case "success":
		return "", nil
	case "need_captcha":
		m := g.Mobile
		msg := "短信通道需要安全验证，请完成滑块验证后自动发送"
		if m != "" && strings.Contains(m, "携") {
			msg = "该号码为携号转网号码（" + m + "），需完成滑块验证后发送验证码"
		}
		s.setStage(carrier.StageNeedCaptcha, msg)
		return m, nil
	default:
		msg := g.Msg
		if msg == "" {
			msg = "服务端未知错误"
		}
		return "", fmt.Errorf("%s", msg)
	}
}

// ---------- carrier.LoginSession / CaptchaSession ----------

// SubmitCaptcha 前端滑块完成后注入票据（非阻塞，校验在会话 goroutine 内完成）
func (s *unicomSession) SubmitCaptcha(ticket, randstr string) error {
	if ticket == "" || randstr == "" {
		return fmt.Errorf("滑块票据为空，请重新完成滑块验证")
	}
	s.mu.Lock()
	busy := s.capBusy
	s.mu.Unlock()
	if busy {
		return fmt.Errorf("滑块校验进行中，请稍候")
	}
	select {
	case s.capCh <- [2]string{ticket, randstr}:
		s.mu.Lock()
		s.capBusy = true
		s.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("已有待处理的滑块票据")
	}
}

// SubmitCode 提交短信验证码（非阻塞，登录在会话 goroutine 内完成）
func (s *unicomSession) SubmitCode(code string) error {
	s.mu.Lock()
	submitted := s.submitted
	s.mu.Unlock()
	if submitted {
		return fmt.Errorf("验证码已提交过，请等待登录完成")
	}
	select {
	case s.codeCh <- code:
		return nil
	default:
		return fmt.Errorf("验证码已提交过，请等待登录完成")
	}
}

func (s *unicomSession) Cancel() { s.cancel() }

func (s *unicomSession) Done() <-chan struct{} { return s.done }

func (s *unicomSession) Status() (stage, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.msg
}

func (s *unicomSession) CodeSubmitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted
}

// ---------- carrier.LoginStateSaver ----------

// SaveLogin 会话成功后由 Web 层调用：回写登录态
// （Cookie=ecs_token 查询凭证；Token=onlin_token 会话维持；AppID=联通 APP 身份）
func (s *unicomSession) SaveLogin(a *store.Account) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state == nil {
		return
	}
	a.Cookie = state.Cookie
	a.Token = state.TokenOnline
	a.AppID = state.AppID
}

// ---------- 内部 ----------

func (s *unicomSession) setStage(stage, msg string) {
	s.mu.Lock()
	s.stage = stage
	s.msg = msg
	s.mu.Unlock()
}
