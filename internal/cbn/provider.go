package cbn

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国广电接入实现（短信验证码 HTTP 登录 + Cookie 查询）
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Cbn }
func (Provider) Name() string        { return "中国广电" }
func (Provider) NeedsPassword() bool { return false }
func (Provider) NeedsSMSCode() bool  { return true }
func (Provider) NeedsOpenID() bool   { return false }

// Query 查询单个广电账号（Cookie 会话；过期需重新登录）
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	return QueryPhone(acc, dataDir, log)
}

// StartLogin 启动图片验证码 + 短信验证码登录会话（HTTP，无浏览器）
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return StartLogin(p.Phone, p.Log)
}
