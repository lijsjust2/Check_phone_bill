package mobile

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国移动接入实现（登录/查询核心逻辑在 login.go / query.go）
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Mobile }
func (Provider) Name() string        { return "中国移动" }
func (Provider) NeedsPassword() bool { return false }
func (Provider) NeedsSMSCode() bool  { return true }

// Query 查询单个移动账号
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	pr := QueryPhone(acc.Phone, dataDir, log)
	return &carrier.Result{
		Phone:  pr.Phone,
		Result: pr.Result,
		Raw:    pr.Raw,
		JSONFn: pr.JSONFn,
		Err:    pr.Err,
	}
}

// StartLogin 启动浏览器验证码登录流程
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return StartLogin(p.Phone, p.DataDir, p.Headless, p.Log)
}
