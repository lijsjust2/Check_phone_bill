package unicom

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国联通接入实现（短信验证码登录：纯 HTTP，无浏览器/滑块/VNC）
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Unicom }
func (Provider) Name() string        { return "中国联通" }
func (Provider) NeedsPassword() bool { return false }
func (Provider) NeedsSMSCode() bool  { return true }

// Query 查询单个联通账号（会话 Cookie 直连 m.client，失效提示重新短信登录）
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	return QueryPhone(acc, dataDir, log)
}

// StartLogin 启动短信验证码登录会话：向手机下发验证码，用户输入后凭会话 Cookie 完成登录
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return StartLogin(p.Phone, p.Log)
}
