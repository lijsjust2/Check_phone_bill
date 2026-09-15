package carrier

import (
	"fmt"
	"sync"
	"time"
)

// SessionMeta 会话元信息（活跃中或已结束）
type SessionMeta struct {
	Phone     string
	Carrier   string
	Stage     string
	Msg       string
	Submitted bool
}

// SessionManager 登录会话管理器：同一时刻仅一个登录会话
// （浏览器型需串行避免 Chromium profile 锁），并记录最近一次结束的会话供前端展示原因。
type SessionManager struct {
	mu      sync.Mutex
	session LoginSession
	meta    SessionMeta // 活跃会话信息
	last    SessionMeta // 最近一次结束的会话
	onDone  func(m SessionMeta, sess LoginSession)
}

// NewSessionManager 创建会话管理器；onDone 在会话结束时回调（meta 为结束时状态，sess 可 type-assert LoginStateSaver）
func NewSessionManager(onDone func(m SessionMeta, sess LoginSession)) *SessionManager {
	return &SessionManager{onDone: onDone}
}

// Start 启动新会话：先取消旧会话并等待结束（释放浏览器 profile 锁，上限 15 秒）
func (m *SessionManager) Start(p Provider, params LoginParams) error {
	m.mu.Lock()
	if m.session != nil {
		old := m.session
		old.Cancel()
		m.mu.Unlock()
		select {
		case <-old.Done():
		case <-time.After(15 * time.Second):
		}
		m.mu.Lock()
	}
	m.mu.Unlock()

	sess, err := p.StartLogin(params)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.session = sess
	m.meta = SessionMeta{Phone: params.Phone, Carrier: p.Code()}
	m.last = SessionMeta{}
	m.mu.Unlock()

	go func() {
		<-sess.Done()
		stage, msg := sess.Status()
		m.mu.Lock()
		if m.session == sess {
			m.session = nil
		}
		meta := m.meta
		meta.Stage, meta.Msg, meta.Submitted = stage, msg, sess.CodeSubmitted()
		m.last = meta
		m.mu.Unlock()
		if m.onDone != nil {
			m.onDone(meta, sess)
		}
	}()
	return nil
}

// Status 当前活跃会话；无活跃会话时返回最近一次结束会话的元信息（active=false）
func (m *SessionManager) Status() (active bool, meta SessionMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		stage, msg := m.session.Status()
		meta = m.meta
		meta.Stage, meta.Msg = stage, msg
		return true, meta
	}
	return false, m.last
}

// StatusDetail 当前活跃会话与元信息：额外返回会话实例，
// 供 Web 层提取运营商特定数据（如电信设备注册的图片验证码）
func (m *SessionManager) StatusDetail() (active bool, meta SessionMeta, sess LoginSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil {
		stage, msg := m.session.Status()
		meta = m.meta
		meta.Stage, meta.Msg = stage, msg
		return true, meta, m.session
	}
	return false, m.last, nil
}

// SubmitCode 向活跃会话提交验证码
func (m *SessionManager) SubmitCode(code string) error {
	m.mu.Lock()
	sess := m.session
	m.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("当前没有进行中的登录会话")
	}
	return sess.SubmitCode(code)
}

// SubmitImageCaptcha 向活跃会话提交图片验证码（仅电信设备注册会话支持）
func (m *SessionManager) SubmitImageCaptcha(captcha string) error {
	m.mu.Lock()
	sess := m.session
	m.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("当前没有进行中的登录会话")
	}
	cs, ok := sess.(ImageCaptchaSession)
	if !ok {
		return fmt.Errorf("当前登录会话不需要图片验证码")
	}
	return cs.SubmitImageCaptcha(captcha)
}

// Cancel 取消活跃会话
func (m *SessionManager) Cancel() {
	m.mu.Lock()
	sess := m.session
	m.mu.Unlock()
	if sess != nil {
		sess.Cancel()
	}
}

// CancelAndWait 取消活跃会话并等待结束（释放浏览器 profile 文件锁，上限 15 秒）
func (m *SessionManager) CancelAndWait() {
	m.mu.Lock()
	sess := m.session
	m.mu.Unlock()
	if sess == nil {
		return
	}
	sess.Cancel()
	select {
	case <-sess.Done():
	case <-time.After(15 * time.Second):
	}
}
