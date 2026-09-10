package unicom

import (
	"testing"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/store"
)

// 依据开源脚本（10010v4）解析逻辑构造的余量响应样例
// （流量原始单位 KB；语音分钟；短信条数）
const sampleFlowLeft = `{
  "code": "0000",
  "time": "2024.01.01 12:00:00",
  "packageName": "5G畅爽冰激凌套餐-99元",
  "summary": {"sum": "7366923.0", "freeFlow": "0"},
  "resources": [
    {
      "type": "flow",
      "details": [
        {"feePolicyName": "套餐内流量", "total": "29360128", "remain": "22093205", "use": "7266923", "limited": "0"}
      ]
    },
    {
      "type": "voice",
      "details": [
        {"feePolicyName": "套餐内通话", "total": "2250", "remain": "2211", "use": "39", "limited": "0"}
      ]
    }
  ],
  "unshared": [
    {
      "type": "unsharedFlowList",
      "details": [
        {"feePolicyName": "非共享流量", "addupItemCode": "40008", "total": "1048576", "remain": "1000000", "use": "10000", "limited": "0"}
      ]
    }
  ],
  "rzbresources": [
    {
      "type": "rzb",
      "details": [
        {"feePolicyName": "日租宝", "total": "0", "remain": "0", "use": "100000", "limited": "1"}
      ]
    }
  ],
  "mlresources": [
    {
      "type": "ml",
      "details": [
        {"feePolicyName": "免流流量（定向）", "total": "3145728", "remain": "3145728", "use": "0", "limited": "0"}
      ]
    }
  ],
  "smslist": [
    {
      "type": "smslist",
      "details": [
        {"feePolicyName": "套餐内短信", "total": "100", "remain": "95", "use": "5", "limited": "0"}
      ]
    }
  ]
}`

func TestParseFlowLeft(t *testing.T) {
	r := ParseFlowLeft(gjson.Parse(sampleFlowLeft))

	if r.Carrier != carrier.Unicom {
		t.Errorf("carrier = %q", r.Carrier)
	}
	if r.PlanName != "5G畅爽冰激凌套餐-99元" {
		t.Errorf("planName = %q", r.PlanName)
	}
	// 通用流量 = resources(7266923/29360128) + unshared(10000/1048576)
	// 已用 7276923KB = 6.94GB；总量 30408704KB = 29.00GB
	if r.GeneralFlow.Used != "6.94GB" {
		t.Errorf("generalFlow used = %q", r.GeneralFlow.Used)
	}
	if r.GeneralFlow.TotalNum < 28.99 || r.GeneralFlow.TotalNum > 29.01 {
		t.Errorf("generalFlow totalNum = %v", r.GeneralFlow.TotalNum)
	}
	// 区域流量（日租宝）：不限量 + 已用 100000KB
	if r.RegionalFlow.Total != "不限量" || !r.RegionalFlow.Unlimited {
		t.Errorf("regionalFlow total = %q unlimited=%v", r.RegionalFlow.Total, r.RegionalFlow.Unlimited)
	}
	if r.RegionalFlow.UsedNum < 0.095 || r.RegionalFlow.UsedNum > 0.096 {
		t.Errorf("regionalFlow usedNum = %v", r.RegionalFlow.UsedNum)
	}
	// 定向流量（免流）：3145728KB = 3GB
	if r.SpecialFlow.Total != "3GB" || r.SpecialFlow.Used != "0GB" {
		t.Errorf("specialFlow = %q / %q", r.SpecialFlow.Used, r.SpecialFlow.Total)
	}
	// 总流量：summary.sum 7366923KB = 7.03GB；总量 = 通用+区域 = 30408704KB
	if r.TotalFlow.Used != "7.03GB" {
		t.Errorf("totalFlow used = %q", r.TotalFlow.Used)
	}
	if r.TotalFlow.TotalNum < 28.99 || r.TotalFlow.TotalNum > 29.01 {
		t.Errorf("totalFlow totalNum = %v", r.TotalFlow.TotalNum)
	}
	// 语音 / 短信
	if r.Voice.Used != "39分钟" || r.Voice.Total != "2250分钟" || r.VoiceRemaining != "2211分钟" {
		t.Errorf("voice = %q / %q / %q", r.Voice.Used, r.Voice.Total, r.VoiceRemaining)
	}
	if r.Sms.Used != "5条" || r.Sms.Total != "100条" || r.SmsRemaining != "95条" {
		t.Errorf("sms = %q / %q / %q", r.Sms.Used, r.Sms.Total, r.SmsRemaining)
	}
	// 已用百分比 ≈ 7.03/29.00 ≈ 24.25%
	if r.FlowUsedPercent < 24.2 || r.FlowUsedPercent > 24.3 {
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

func TestSetCookieToCookie(t *testing.T) {
	sc := "e3d5632b6e5a4f8d=ABC; Domain=.10010.com; Path=/, another=XYZ; Path=/"
	got := setCookieToCookie(sc)
	if got != "e3d5632b6e5a4f8d=ABC; another=XYZ" {
		t.Errorf("setCookieToCookie = %q", got)
	}
}

func TestFmtGB(t *testing.T) {
	cases := []struct {
		kb   float64
		want string
	}{
		{0, "0GB"},
		{1048576, "1GB"},
		{7366923, "7.03GB"},
		{512 * 1024, "0.5GB"},
	}
	for _, c := range cases {
		if got := fmtGB(c.kb); got != c.want {
			t.Errorf("fmtGB(%v) = %q, want %q", c.kb, got, c.want)
		}
	}
}

