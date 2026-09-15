package cbn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// cbnSession 广电登录会话：图片验证码 → 短信验证码 → gwLogin。
// 实现 carrier.LoginSession（短信码经 SubmitCode 注入）与
// carrier.ImageCaptchaSession（图片码经 SubmitImageCaptcha 注入）。
type cbnSession struct {
	phone string
	log   *loggerx.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	captchaCh chan string // 图片验证码输入（need_image_captcha 阶段）
	codeCh    chan string // 短信验证码输入（waiting_code 阶段）

	client *Client

	mu          sync.Mutex
	stage       string
	msg         string
	submitted   bool
	state       *LoginState // 登录成功产物（SaveLogin 用）
	captchaImg  string      // 当前图片验证码 data URI
	smsSent     bool        // 短信已发送（短信码错误重试时不重复发码）
	lastCaptcha string      // 当前图片验证码对应的输入（gwLogin 需回填）
}

// StartLogin 启动广电短信验证码登录会话
func StartLogin(phone string, log *loggerx.Logger) (*cbnSession, error) {
	if !validPhone11(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &cbnSession{
		phone:     phone,
		log:       log,
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		captchaCh: make(chan string, 1),
		codeCh:    make(chan string, 1),
	}
	go s.run()
	return s, nil
}

func (s *cbnSession) run() {
	defer close(s.done)
	s.setStage(carrier.StageStarting, "正在连接中国广电网上营业厅...")

	client, err := NewClient()
	if err != nil {
		s.finishError(err)
		return
	}
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()

	// 过 WAF 建立 cookie 会话
	ctx := s.ctx
	if err := client.EnsureSession(ctx); err != nil {
		s.finishError(fmt.Errorf("连接广电营业厅失败: %w", err))
		return
	}

	// 图片验证码 → 短信验证码 → 登录（图片码错误自动换图重试）
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if s.ctx.Err() != nil {
			s.finishCancelled()
			return
		}
		s.setStage(carrier.StageCodeSending, "正在获取图片验证码...")
		img, err := client.FetchImageCaptcha(ctx)
		if err != nil {
			s.finishError(err)
			return
		}
		s.setCaptchaImage(img)
		s.setStage(carrier.StageNeedImageCaptcha, "请输入图片中的字符")

		captcha, ok := s.waitInput(s.captchaCh)
		if !ok {
			s.finishCancelled()
			return
		}

		// 发送短信验证码（同一会话 cookie 下图片码立即生效校验）
		s.setStage(carrier.StageCodeSending, "正在校验图片验证码并发送短信...")
		if err := client.SendSmsCode(ctx, s.phone, s.log); err != nil {
			// 疑似图片验证码错误：换图重试
			s.setCaptchaImage("")
			s.setStage(carrier.StageNeedImageCaptcha, "发送失败: "+err.Error()+"，请重新输入图片验证码")
			continue
		}
		s.setCaptchaImage("")
		s.mu.Lock()
		s.smsSent = true
		s.lastCaptcha = captcha
		s.mu.Unlock()

		// 短信验证码：最多 3 次，失败后回输入阶段（不重复发码）
		const maxCodeAttempts = 3
		for codeAttempt := 0; codeAttempt < maxCodeAttempts; codeAttempt++ {
			s.setStage(carrier.StageWaitingCode, "短信验证码已发送，请输入收到的验证码")
			code, ok := s.waitInput(s.codeCh)
			if !ok {
				s.finishCancelled()
				return
			}
			s.setStage(carrier.StageSubmitting, "正在登录...")
			s.mu.Lock()
			s.submitted = true
			s.mu.Unlock()
			state, err := client.GwLogin(ctx, s.phone, code, captcha, s.log)
			if err == nil {
				s.finishSuccess(state)
				return
			}
			if codeAttempt < maxCodeAttempts-1 {
				s.setStage(carrier.StageWaitingCode, "登录失败: "+err.Error()+"，请重新输入短信验证码")
			} else {
				s.finishError(fmt.Errorf("短信验证码错误次数过多: %w", err))
				return
			}
		}
	}
	s.finishError(fmt.Errorf("登录失败次数过多，请稍后重试"))
}

// waitInput 等待用户输入（取消时返回 false）
func (s *cbnSession) waitInput(ch chan string) (string, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-s.ctx.Done():
		return "", false
	}
}

// ---------- carrier.LoginSession ----------

// submitInput 提交用户输入：会话在等待对应输入时立即接收（10 秒超时）
func (s *cbnSession) submitInput(ch chan string, v, emptyMsg string) error {
	if v == "" {
		return fmt.Errorf("%s", emptyMsg)
	}
	select {
	case ch <- v:
		return nil
	case <-s.ctx.Done():
		return fmt.Errorf("登录会话已结束")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("当前不在验证码输入阶段，请稍后再试")
	}
}

// SubmitCode 提交短信验证码
func (s *cbnSession) SubmitCode(code string) error {
	return s.submitInput(s.codeCh, code, "请输入短信验证码")
}

// ---------- carrier.ImageCaptchaSession ----------

// SubmitImageCaptcha 提交图片验证码（登录第一步）
func (s *cbnSession) SubmitImageCaptcha(captcha string) error {
	return s.submitInput(s.captchaCh, captcha, "请输入图片验证码")
}

// CaptchaImage 当前图片验证码（data URI，无图时返回空串）
func (s *cbnSession) CaptchaImage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captchaImg
}

func (s *cbnSession) Cancel() { s.cancel() }

func (s *cbnSession) Done() <-chan struct{} { return s.done }

func (s *cbnSession) Status() (stage, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.msg
}

func (s *cbnSession) CodeSubmitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted
}

// ---------- carrier.LoginStateSaver ----------

// SaveLogin 会话成功后由 Web 层调用：回写 sessionId（Token）与 Cookie
func (s *cbnSession) SaveLogin(a *store.Account) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state == nil {
		return
	}
	a.Token = state.SessionID
	a.Cookie = state.CookiesJSON
	a.ProvinceCode = state.PhoneInfo.Get("mgmtProv").Str
	a.CityCode = state.PhoneInfo.Get("areaCode").Str
}

// ---------- 内部 ----------

func (s *cbnSession) setStage(stage, msg string) {
	s.mu.Lock()
	s.stage = stage
	s.msg = msg
	s.mu.Unlock()
}

func (s *cbnSession) setCaptchaImage(img string) {
	s.mu.Lock()
	s.captchaImg = img
	s.mu.Unlock()
}

func (s *cbnSession) finishSuccess(state *LoginState) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.setStage(carrier.StageSuccess, "")
}

func (s *cbnSession) finishError(err error) {
	s.setStage(carrier.StageError, err.Error())
}

func (s *cbnSession) finishCancelled() {
	s.setStage(carrier.StageClosed, "已取消")
}

func validPhone11(p string) bool {
	if len(p) != 11 {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
