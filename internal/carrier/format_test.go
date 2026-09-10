package carrier

import (
	"strings"
	"testing"

	"chinamobile-monitor/internal/store"
)

func TestFormatResultLines(t *testing.T) {
	r := &store.QueryResult{
		City:        "北京",
		Balance:     "18.30元",
		PlanName:    "移动花卡宝藏版",
		RealtimeFee: "5.20",
		TotalFlow:   store.UsageItem{Used: "6.65GB", Total: "60GB"},
		Voice:       store.UsageItem{Used: "120分钟", Total: "200分钟"},
		Sms:         store.UsageItem{Used: "5条", Total: "100条"},
		QueriedAt:   "2026-09-07 12:00:00",
	}
	lines := FormatResultLines("13800138000", r, store.DefaultFields())
	joined := strings.Join(lines, "\n") + "\n"
	for _, want := range []string{
		"手机号：13800138000", "  城市：北京", "  套餐：移动花卡宝藏版",
		"  余额：18.30元", "  实时费用：5.20元",
		"  总流量：已用 6.65GB / 总 60GB",
		"  语音：已用 120分钟 / 总 200分钟",
		"  短信：已用 5条 / 总 100条",
		"  查询时间：2026-09-07 12:00:00",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("输出缺少行: %q\n实际输出:\n%s", want, joined)
		}
	}

	// 运营商标注：非移动结果在首行标注运营商名
	rTelecom := &store.QueryResult{Carrier: Telecom, Balance: "18.30元"}
	lines = FormatResultLines("15300153000", rTelecom, store.DefaultFields())
	if len(lines) == 0 || !strings.Contains(lines[0], "（中国电信）") {
		t.Errorf("电信结果应标注运营商，首行: %q", lines[0])
	}
}

func TestIsAlert(t *testing.T) {
	r := &store.QueryResult{BalanceNum: 5.0}
	if !IsAlert(r, 10, 80) {
		t.Error("余额 5 < 10 应告警")
	}
	r2 := &store.QueryResult{BalanceNum: 50, FlowUsedPercent: 90}
	if !IsAlert(r2, 10, 80) {
		t.Error("流量 90% > 80% 应告警")
	}
	r3 := &store.QueryResult{BalanceNum: 50, FlowUsedPercent: 10}
	if IsAlert(r3, 10, 80) {
		t.Error("无告警条件命中")
	}
}
