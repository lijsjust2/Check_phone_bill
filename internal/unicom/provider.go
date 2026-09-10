package unicom

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国联通接入实现（官网浏览器验证码登录 + Cookie 查询）
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Unicom }
func (Provider) Name() string        { return "中国联通" }
func (Provider) NeedsPassword() bool { return false }
func (Provider) NeedsSMSCode() bool  { return true }

// Query 查询单个联通账号（Cookie 失效自动会话维持刷新）
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	return QueryPhone(acc, dataDir, log)
}

// StartLogin 启动官网验证码登录会话（浏览器自动化：用户完成腾讯滑块后发送随机码）
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return StartLogin(p.Phone, p.DataDir, p.Headless, p.Log)
}
