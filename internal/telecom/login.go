package telecom

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// telecomSession 电信登录会话：服务密码登录；
// 遇 3006 设备未信任时自动进入网关设备注册流程
// （图片验证码 → 短信验证码 → androidId → 携带 androidId 重新登录）。
// 实现 carrier.LoginSession（验证码经 SubmitCode 注入）与 carrier.ImageCaptchaSession。
type telecomSession struct {
	phone     string
	password  string
	androidID string // 已绑定的设备 id（重新登录复用；注册成功后更新）
	dataDir   string
	log       *loggerx.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	captchaCh chan string // 图片验证码输入（need_image_captcha 阶段）
	codeCh    chan string // 短信验证码输入（waiting_code 阶段）

	mu         sync.Mutex
	stage      string
	msg        string
	submitted  bool
	state      *LoginState // 登录成功产物（SaveLogin 用）
	captchaImg string      // 当前图片验证码 data URI（need_image_captcha 阶段展示）
}

// StartLogin 启动电信服务密码登录会话
func StartLogin(phone, password, androidID, dataDir string, log *loggerx.Logger) (*telecomSession, error) {
	if !validPhone11(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	if password == "" {
		return nil, fmt.Errorf("请填写服务密码")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &telecomSession{
		phone:     phone,
		password:  password,
		androidID: androidID,
		dataDir:   dataDir,
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

func (s *telecomSession) run() {
	defer close(s.done)
	s.setStage(carrier.StageStarting, "正在通过服务密码登录中国电信...")

	// 第一次尝试：已绑定设备直接携带 androidId 登录
	state, err := s.tryLogin(s.androidID)
	s.mu.Lock()
	s.submitted = true
	s.mu.Unlock()
	if err == nil {
		s.finishSuccess(state)
		return
	}
	if !errors.Is(err, ErrDeviceUntrusted) {
		s.finishError(err)
		return
	}
	if s.ctx.Err() != nil {
		s.finishCancelled()
		return
	}

	// 3006 设备未信任：进入网关设备注册（图片验证码 + 短信验证码）
	if !s.enrollFlow() {
		return // 结束状态已在内部设置
	}

	// 注册成功：携带新 androidId 重新登录
	s.setStage(carrier.StageStarting, "设备绑定成功，正在重新登录...")
	state, err = s.tryLogin(s.androidID)
	if err != nil {
		s.finishError(err)
		return
	}
	s.finishSuccess(state)
}

// tryLogin 执行一次密码登录（ErrDeviceUntrusted 时由调用方进入注册流程）
func (s *telecomSession) tryLogin(androidID string) (*LoginState, error) {
	return DoLogin(s.ctx, s.phone, s.password, androidID, s.log)
}

// enrollFlow 网关设备注册：获取图片验证码 → 用户输入 → 发短信 → 用户输入 → 换取 androidId。
// 返回 false 表示流程失败/取消（结束状态已设置）。
func (s *telecomSession) enrollFlow() bool {
	const maxAttempts = 5 // 整轮重试次数（图片验证码错误/短信码连续错误后重新开始）
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if s.ctx.Err() != nil {
			s.finishCancelled()
			return false
		}
		s.setStage(carrier.StageCodeSending, "首次登录需短信验证绑定设备，正在获取图片验证码...")
		cap, err := FetchEnrollCaptcha(s.ctx, s.phone, s.log)
		if err != nil {
			s.finishError(fmt.Errorf("获取图片验证码失败: %w", err))
			return false
		}
		s.setCaptchaImage(cap.Image)
		s.setStage(carrier.StageNeedImageCaptcha, "请输入图片中的 4 位字符")

		captcha, ok := s.waitInput(s.captchaCh)
		if !ok {
			s.finishCancelled()
			return false
		}
		s.setStage(carrier.StageCodeSending, "正在校验图片验证码并发送短信...")
		flow, err := SendEnrollCode(s.ctx, cap.Flow, captcha, s.log)
		if err != nil {
			// 图片验证码错误：重新获取新图重试
			s.setCaptchaImage("")
			s.setStage(carrier.StageNeedImageCaptcha, "图片验证码错误: "+err.Error()+"，请重新输入")
			continue
		}

		// 短信验证码阶段：最多尝试 3 次，失败后回到图片验证码重新走流程
		const maxCodeAttempts = 3
		for codeAttempt := 0; codeAttempt < maxCodeAttempts; codeAttempt++ {
			s.setStage(carrier.StageWaitingCode, "短信验证码已发送，请输入收到的验证码")
			code, ok := s.waitInput(s.codeCh)
			if !ok {
				s.finishCancelled()
				return false
			}
			s.setStage(carrier.StageSubmitting, "正在完成设备绑定...")
			androidID, err := EnrollLogin(s.ctx, flow, code, s.log)
			if err == nil {
				s.mu.Lock()
				s.androidID = androidID
				s.mu.Unlock()
				return true
			}
			if codeAttempt < maxCodeAttempts-1 {
				s.setStage(carrier.StageWaitingCode, "验证失败: "+err.Error()+"，请重新输入短信验证码")
			}
		}
		s.setCaptchaImage("")
	}
	s.finishError(errors.New("设备绑定失败次数过多，请稍后重试"))
	return false
}

// waitInput 等待用户输入（取消时返回 false）
func (s *telecomSession) waitInput(ch chan string) (string, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-s.ctx.Done():
		return "", false
	}
}

// ---------- carrier.LoginSession ----------

// submitInput 提交用户输入：会话在等待对应输入时立即接收
func (s *telecomSession) submitInput(ch chan string, v, emptyMsg string) error {
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

// SubmitCode 提交短信验证码（设备注册短信登录阶段）
func (s *telecomSession) SubmitCode(code string) error {
	return s.submitInput(s.codeCh, code, "请输入短信验证码")
}

// ---------- carrier.ImageCaptchaSession ----------

// SubmitImageCaptcha 提交图片验证码（设备注册第一步）
func (s *telecomSession) SubmitImageCaptcha(captcha string) error {
	return s.submitInput(s.captchaCh, captcha, "请输入图片验证码")
}

// CaptchaImage 当前图片验证码（data URI，无图时返回空串）
func (s *telecomSession) CaptchaImage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captchaImg
}

func (s *telecomSession) Cancel() { s.cancel() }

func (s *telecomSession) Done() <-chan struct{} { return s.done }

func (s *telecomSession) Status() (stage, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.msg
}

func (s *telecomSession) CodeSubmitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted
}

// ---------- carrier.LoginStateSaver ----------

// SaveLogin 会话成功后由 Web 层调用：回写服务密码、token 与设备 androidId
func (s *telecomSession) SaveLogin(a *store.Account) {
	s.mu.Lock()
	state := s.state
	androidID := s.androidID
	s.mu.Unlock()
	if state == nil {
		return
	}
	a.Password = s.password
	a.Token = state.Token
	a.ProvinceCode = state.ProvinceCode
	a.CityCode = state.CityCode
	if androidID != "" {
		a.AndroidID = androidID
	}
}

// ---------- 内部 ----------

func (s *telecomSession) setStage(stage, msg string) {
	s.mu.Lock()
	s.stage = stage
	s.msg = msg
	s.mu.Unlock()
}

func (s *telecomSession) setCaptchaImage(img string) {
	s.mu.Lock()
	s.captchaImg = img
	s.mu.Unlock()
}

func (s *telecomSession) finishSuccess(state *LoginState) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.setStage(carrier.StageSuccess, "")
}

func (s *telecomSession) finishError(err error) {
	s.setStage(carrier.StageError, err.Error())
}

func (s *telecomSession) finishCancelled() {
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
