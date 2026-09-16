package unicom

import (
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// Provider 中国联通接入实现（微信小程序 OpenID 通道，ha_unicom_bill 协议）：
// 凭证为长期稳定的 OpenID，登录即验证，查询时现取 ticket，全程纯 HTTP。
type Provider struct{}

func init() { carrier.Register(Provider{}) }

func (Provider) Code() string        { return carrier.Unicom }
func (Provider) Name() string        { return "中国联通" }
func (Provider) NeedsPassword() bool { return false }
func (Provider) NeedsSMSCode() bool  { return false }
func (Provider) NeedsOpenID() bool   { return true }

// Query 查询单个联通账号（OpenID 现取 ticket 直连 mxx.client 掌厅接口）
func (Provider) Query(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	return QueryPhone(acc, dataDir, log)
}

// StartLogin 启动 OpenID 验证登录会话（getTicket + serviceEntrance，秒级完成）
func (Provider) StartLogin(p carrier.LoginParams) (carrier.LoginSession, error) {
	return startWxLogin(p.Phone, p.OpenID, p.Log)
}
