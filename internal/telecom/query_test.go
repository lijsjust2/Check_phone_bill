package telecom

import (
	"testing"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
)

// 依据 Cp0204/ChinaTelecomMonitor README 的 /summary 接口字段构造的响应样例
// （flowInfo 单位 KB、voiceInfo 单位分钟、balanceInfo 单位元）
const sampleImportantData = `{
  "flowInfo": {
    "totalAmount": {"used": 7366923, "balance": 24000000, "over": 0},
    "commonFlow": {"used": 7273962, "balance": 18276484, "over": 0},
    "specialAmount": {"used": 92961, "balance": 215172319}
  },
  "voiceInfo": {"voiceDataInfo": {"used": 39, "balance": 2211, "total": 2250}},
  "balanceInfo": {"indexBalanceDataInfo": {"balance": "10.05"}},
  "provinceName": "浙江"
}`

func TestParseImportantData(t *testing.T) {
	r := ParseImportantData(gjson.Parse(sampleImportantData))

	if r.Carrier != carrier.Telecom {
		t.Errorf("carrier = %q", r.Carrier)
	}
	if r.Balance != "10.05元" || r.BalanceNum != 10.05 {
		t.Errorf("balance = %q / %v", r.Balance, r.BalanceNum)
	}
	if r.City != "浙江" {
		t.Errorf("city = %q", r.City)
	}
	// 总流量：7366923KB = 7.03GB；total = (7366923+24000000)KB = 29.91GB
	if r.TotalFlow.Used != "7.03GB" || r.TotalFlow.Total != "29.91GB" {
		t.Errorf("totalFlow = %q / %q", r.TotalFlow.Used, r.TotalFlow.Total)
	}
	if r.TotalFlow.UsedNum < 7.02 || r.TotalFlow.UsedNum > 7.04 {
		t.Errorf("totalFlow usedNum = %v", r.TotalFlow.UsedNum)
	}
	// 通用流量
	if r.GeneralFlow.Used != "6.94GB" || r.GeneralFlow.Total != "24.37GB" {
		t.Errorf("generalFlow = %q / %q", r.GeneralFlow.Used, r.GeneralFlow.Total)
	}
	// 定向流量
	if r.SpecialFlow.Used != "0.09GB" || r.SpecialFlow.Total != "205.29GB" {
		t.Errorf("specialFlow = %q / %q", r.SpecialFlow.Used, r.SpecialFlow.Total)
	}
	// 语音
	if r.Voice.Used != "39分钟" || r.Voice.Total != "2250分钟" || r.VoiceRemaining != "2211分钟" {
		t.Errorf("voice = %q / %q / remain %q", r.Voice.Used, r.Voice.Total, r.VoiceRemaining)
	}
	// 已用百分比 = 7366923/31366923 ≈ 23.48%
	if r.FlowUsedPercent < 23.47 || r.FlowUsedPercent > 23.49 {
		t.Errorf("flowPercent = %v", r.FlowUsedPercent)
	}
}

func TestParseImportantDataEmpty(t *testing.T) {
	// 空对象：字段留空，不 panic
	r := ParseImportantData(gjson.Parse(`{}`))
	if r.Balance != "" || r.TotalFlow.Used != "" {
		t.Errorf("空响应应全部留空, got %+v", r)
	}
	if r.FlowUsedPercent != 0 {
		t.Errorf("flowPercent = %v", r.FlowUsedPercent)
	}
}

func TestTransNumber(t *testing.T) {
	// 编码 +2 后再解码 -2 应还原
	s := "18912345678"
	if enc := TransNumber(s, true); enc == s {
		t.Errorf("编码后不应等于原文")
	}
	if dec := TransNumber(TransNumber(s, true), false); dec != s {
		t.Errorf("编解码还原失败: %q", dec)
	}
}

func TestFmtGB(t *testing.T) {
	cases := []struct {
		kb   float64
		want string
	}{
		{0, "0GB"},
		{1048576, "1GB"},
		{1048576 * 30, "30GB"},
		{7366923, "7.03GB"},    // 7.0253
		{512 * 1024, "0.50GB"}, // 0.5
	}
	for _, c := range cases {
		if got := fmtGB(c.kb); got != c.want {
			t.Errorf("fmtGB(%v) = %q, want %q", c.kb, got, c.want)
		}
	}
}
