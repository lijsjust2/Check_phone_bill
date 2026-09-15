package unicom

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国联通接入实现（网页浏览器登录 + JUT Cookie 查询）
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Unicom }
func (Provider) Name() string        { return "中国联通" }
func (Provider) NeedsPassword() bool { return false }
func (Provider) NeedsSMSCode() bool  { return false }

// Query 查询单个联通账号（JUT Cookie 直连 mxx 域，失效提示重新网页登录）
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	return QueryPhone(acc, dataDir, log)
}

// StartLogin 启动网页登录会话：弹出真浏览器打开联通网厅，
// 用户在窗口内自行完成登录（滑块/验证码均在真实浏览器内），面板监测 JUT Cookie
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return StartLogin(p.Phone, p.DataDir, p.Headless, p.Log)
}
