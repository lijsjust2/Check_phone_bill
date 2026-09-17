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

// 联通登录会话（微信小程序 OpenID 通道，ha_unicom_bill 协议）。
//
// OpenID 是长期稳定凭证（用户在微信「中国联通」小程序登录后产生），
// "登录" = 用 OpenID 现取 ticket 并验证可用：
//  1. getTicket 验证 OpenID 有效
//  2. serviceEntrance 换取掌厅会话 Cookie（microHallUser / microHallAccessToken）
//  3. queryGoodsList 读取该 OpenID 名下完整手机号，与输入号码比对（尽力校验）
//
// 成功后把 OpenID 回写到账号；之后每次查询现取 ticket，不存在"登录态过期"。
// 无浏览器、无短信、无滑块，全程纯 HTTP。

// wxSession OpenID 登录会话（验证型，秒级完成）
type wxSession struct {
	phone  string
	openid string
	log    *loggerx.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	stage string
	msg   string
	state *wxLoginState
}

// wxLoginState 登录成功产物（SaveLogin 回写账号）
type wxLoginState struct {
	OpenID string
}

// startWxLogin 启动 OpenID 验证登录会话
func startWxLogin(phone, openid string, log *loggerx.Logger) (*wxSession, error) {
	if !store.ValidPhone(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	openid = strings.TrimSpace(openid)
	if openid == "" {
		return nil, fmt.Errorf("请填写微信小程序 OpenID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	s := &wxSession{
		phone:  phone,
		openid: openid,
		log:    log,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.run()
	return s, nil
}

func (s *wxSession) run() {
	defer close(s.done)
	s.setStage(carrier.StageStarting, "正在验证 OpenID...")

	// 1. OpenID → ticket（无效会返回 ErrOpenIDInvalid）
	ticket, err := getTicket(s.ctx, s.openid)
	if err != nil {
		s.finishError(err)
		return
	}

	// 2. ticket → 掌厅会话 Cookie（尽力而为，失败不阻断）
	cookie := serviceEntrance(s.ctx, ticket, s.log)
	if s.log != nil {
		if cookie != "" {
			s.log.Info("[%s] 联通 OpenID 验证通过，已获取掌厅会话", s.phone)
		} else {
			s.log.Info("[%s] 联通 OpenID 验证通过（serviceEntrance 未返回 Cookie，查询时会重试）", s.phone)
		}
	}

	// 3. 尽力校验：OpenID 名下完整手机号与输入一致
	if p := queryGoodsPhone(s.ctx, s.openid); p != "" && p != s.phone {
		s.finishError(fmt.Errorf("该 OpenID 绑定的号码是 %s，与输入的手机号 %s 不一致，请检查后重试",
			carrier.MaskPhone(p), carrier.MaskPhone(s.phone)))
		return
	}

	s.mu.Lock()
	s.state = &wxLoginState{OpenID: s.openid}
	s.mu.Unlock()
	s.setStage(carrier.StageSuccess, "")
}

// ---------- carrier.LoginSession ----------

// SubmitCode OpenID 通道无需短信验证码
func (s *wxSession) SubmitCode(string) error {
	return fmt.Errorf("联通为 OpenID 登录，无需短信验证码")
}

func (s *wxSession) Cancel() { s.cancel() }

func (s *wxSession) Done() <-chan struct{} { return s.done }

func (s *wxSession) Status() (stage, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.msg
}

func (s *wxSession) CodeSubmitted() bool { return false }

// ---------- carrier.LoginStateSaver ----------

// SaveLogin 会话成功后由 Web 层调用：回写 OpenID 并清掉旧通道遗留凭证
func (s *wxSession) SaveLogin(a *store.Account) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state == nil {
		return
	}
	a.OpenID = state.OpenID
	a.Cookie, a.Token, a.WebToken, a.AppID = "", "", "", ""
}

// ---------- 内部 ----------

func (s *wxSession) setStage(stage, msg string) {
	s.mu.Lock()
	s.stage = stage
	s.msg = msg
	s.mu.Unlock()
}

func (s *wxSession) finishError(err error) {
	if s.log != nil {
		s.log.Error("[%s] 联通登录失败: %v", s.phone, err)
	}
	s.setStage(carrier.StageError, err.Error())
}
