package unicom

import (
	"testing"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/store"
)

// 真实账号（联通王卡）2026-09-10 queryOcsPackageFlowLeftContentRevisedInJune 响应
// （关键字段截取；流量原始单位 MB：flowtype 1=通用 2=专属 3=其他/免流）
const sampleFlowLeft = `{
  "code": "0000",
  "packageName": "联通王卡（新）",
  "allUserFlow": "3130.70",
  "canUseFlowAll": "19.27",
  "canuseFlowAllUnit": "GB",
  "flowSumList": [
    {"elemtype": "3", "flowtype": "1", "xcanusevalue": "19732.89", "xusedvalue": "747.11"},
    {"elemtype": "3", "flowtype": "2", "xcanusevalue": "28336.39", "xusedvalue": "2383.61"},
    {"elemtype": "3", "flowtype": "3", "xcanusevalue": "0.00", "xusedvalue": "0.12"}
  ],
  "voiceHeadUsed": 16,
  "voiceSumresource": 0,
  "canuseVoiceAllUnit": "分钟",
  "smsHeadUsed": 0,
  "smsSumresource": 0,
  "resources": [
    {"details": [
      {"feePolicyName": "30GB联通王卡(新)专属流量包", "flowType": "2", "total": "30720.00", "remain": "28336.39", "use": "2383.60"},
      {"feePolicyName": "广东联通王卡专属10元资源包（20GB通用流量）", "flowType": "1", "total": "20480.00", "remain": "19732.89", "use": "747.10"}
    ], "type": "flow", "userResource": "3130.7002"},
    {"details": [], "type": "Voice", "userResource": "0"},
    {"details": [], "type": "smsList", "userResource": "0"}
  ]
}`

func TestParseFlowLeft(t *testing.T) {
	r := ParseFlowLeft(gjson.Parse(sampleFlowLeft))

	if r.Carrier != carrier.Unicom {
		t.Errorf("carrier = %q", r.Carrier)
	}
	if r.PlanName != "联通王卡（新）" {
		t.Errorf("planName = %q", r.PlanName)
	}
	// 通用流量：flowtype=1 已用 747.11MB≈0.73GB / 总 (747.11+19732.89)=20480MB=20GB
	if r.GeneralFlow.Used != "0.73GB" {
		t.Errorf("generalFlow used = %q", r.GeneralFlow.Used)
	}
	if r.GeneralFlow.Total != "20GB" {
		t.Errorf("generalFlow total = %q", r.GeneralFlow.Total)
	}
	// 定向流量：flowtype=2 已用 2383.61MB≈2.33GB / 总 30720MB=30GB
	if r.SpecialFlow.Used != "2.33GB" {
		t.Errorf("specialFlow used = %q", r.SpecialFlow.Used)
	}
	if r.SpecialFlow.Total != "30GB" {
		t.Errorf("specialFlow total = %q", r.SpecialFlow.Total)
	}
	// 总流量：已用 allUserFlow 3130.70MB≈3.06GB / 总 (20480+30720)MB=50GB
	if r.TotalFlow.Used != "3.06GB" {
		t.Errorf("totalFlow used = %q", r.TotalFlow.Used)
	}
	if r.TotalFlow.Total != "50GB" {
		t.Errorf("totalFlow total = %q", r.TotalFlow.Total)
	}
	// 王卡无套内语音：voiceHeadUsed=16 分钟套外已用，voiceSumresource=0
	if r.Voice.Used != "16分钟" || r.Voice.Total != "0分钟" {
		t.Errorf("voice = %q / %q", r.Voice.Used, r.Voice.Total)
	}
	// 短信 0/0：不落字段
	if r.Sms != (store.UsageItem{}) {
		t.Errorf("sms = %+v", r.Sms)
	}
	// 已用百分比 3.06/50 ≈ 6.1%
	if r.FlowUsedPercent < 6.0 || r.FlowUsedPercent > 6.2 {
		t.Errorf("flowPercent = %v", r.FlowUsedPercent)
	}
}

func TestParseFlowLeftEmpty(t *testing.T) {
	// 空对象：字段留空，不 panic
	r := ParseFlowLeft(gjson.Parse(`{}`))
	if r.PlanName != "" || r.TotalFlow.Used != "" {
		t.Errorf("空响应应全部留空, got %+v", r)
	}
	if r.FlowUsedPercent != 0 {
		t.Errorf("flowPercent = %v", r.FlowUsedPercent)
	}
}

func TestParseBalance(t *testing.T) {
	// accountBalancenew 响应样例（字段参考 ha_unicom_bill 传感器取值）
	sample := `{
	  "code": "0000",
	  "data": {
	    "curntbalancecust": "23.45",
	    "canusefeecustNew": "20.15",
	    "totalrealfee": "36.80",
	    "allbowefeecust": "0"
	  }
	}`
	r := &store.QueryResult{}
	ParseBalance(r, gjson.Parse(sample))
	if r.Balance != "23.45元" || r.BalanceNum != 23.45 {
		t.Errorf("Balance = %q/%v", r.Balance, r.BalanceNum)
	}
	if r.RealtimeFee != "36.80" {
		t.Errorf("RealtimeFee = %q", r.RealtimeFee)
	}

	// 无 data 包裹（顶层即字段）
	r2 := &store.QueryResult{}
	ParseBalance(r2, gjson.Parse(`{"code":"0000","curntbalancecust":"-1.2","realfeecustnew":"5"}`))
	if r2.Balance != "-1.20元" || r2.BalanceNum != -1.2 {
		t.Errorf("Balance2 = %q/%v", r2.Balance, r2.BalanceNum)
	}
	if r2.RealtimeFee != "5.00" {
		t.Errorf("RealtimeFee2 = %q", r2.RealtimeFee)
	}

	// 空响应：字段留空，不 panic
	r3 := &store.QueryResult{}
	ParseBalance(r3, gjson.Parse(`{}`))
	if r3.Balance != "" || r3.RealtimeFee != "" {
		t.Errorf("空响应应留空, got %q/%q", r3.Balance, r3.RealtimeFee)
	}
}

func TestFmtMB(t *testing.T) {
	cases := []struct {
		mb   float64
		want string
	}{
		{0, "0MB"},
		{1024, "1GB"},
		{747.11, "0.73GB"},
		{20480, "20GB"},
		{3130.70, "3.06GB"},
		{0.12, "0.12MB"},
	}
	for _, c := range cases {
		if got := fmtMB(c.mb); got != c.want {
			t.Errorf("fmtMB(%v) = %q, want %q", c.mb, got, c.want)
		}
	}
}
