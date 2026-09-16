package telecom

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国电信接入实现（服务密码 HTTP 登录 + token 查询）
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Telecom }
func (Provider) Name() string        { return "中国电信" }
func (Provider) NeedsPassword() bool { return true }
func (Provider) NeedsSMSCode() bool  { return false }
func (Provider) NeedsOpenID() bool   { return false }

// Query 查询单个电信账号（token 失效自动重登）
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	return QueryPhone(acc, dataDir, log)
}

// StartLogin 启动服务密码登录会话（HTTP，无浏览器；androidID 非空时复用已绑定设备）
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return StartLogin(p.Phone, p.Password, p.AndroidID, p.DataDir, p.Log)
}
