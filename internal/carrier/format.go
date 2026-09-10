package carrier

import (
	"bytes"
	"fmt"

	"chinamobile-monitor/internal/store"
)

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// FormatResultLines 单号结果 → 简洁可读文本（运营商标注 + 输出字段开关）
func FormatResultLines(phone string, res *store.QueryResult, fields store.Fields) []string {
	head := fmt.Sprintf("手机号：%s", phone)
	// 三网合一后按结果中的运营商字段标注（移动结果不带标注保持向后兼容）
	if res != nil && res.Carrier != "" && res.Carrier != Mobile {
		if name := CarrierName(res.Carrier); name != "" {
			head += "（" + name + "）"
		}
	}
	lines := []string{head}

	if fields.City && res.City != "" && res.City != "未知" {
		lines = append(lines, "  城市："+res.City)
	}
	if fields.PlanName {
		lines = append(lines, "  套餐："+orDefault(res.PlanName, "未知"))
	}
	if fields.Balance {
		lines = append(lines, "  余额："+orDefault(res.Balance, "未知"))
	}
	if fields.RealtimeFee && res.RealtimeFee != "" {
		lines = append(lines, "  实时费用："+res.RealtimeFee+"元")
	}
	if fields.BillTotal && res.BillTotal != "" {
		lines = append(lines, "  本月账单："+res.BillTotal+"元")
	}
	if fields.BillReal && res.BillReal != "" {
		lines = append(lines, "  实际应缴："+res.BillReal+"元")
	}
	if fields.DiscountTotal && res.DiscountTotal != "" {
		lines = append(lines, "  优惠合计："+res.DiscountTotal+"元")
	}
	if fields.BillCycle && res.BillCycle != "" && res.BillCycle != " ~ " {
		lines = append(lines, "  账单周期："+res.BillCycle)
	}
	if fields.GeneralFlow && res.GeneralFlow != (store.UsageItem{}) {
		lines = append(lines, fmt.Sprintf("  通用流量：已用 %s / 总 %s", res.GeneralFlow.Used, res.GeneralFlow.Total))
	}
	if fields.SpecialFlow && res.SpecialFlow != (store.UsageItem{}) {
		lines = append(lines, fmt.Sprintf("  定向流量：已用 %s / 总 %s", res.SpecialFlow.Used, res.SpecialFlow.Total))
	}
	if fields.RegionalFlow && res.RegionalFlow != (store.UsageItem{}) {
		lines = append(lines, fmt.Sprintf("  区域流量：已用 %s / 总 %s", res.RegionalFlow.Used, res.RegionalFlow.Total))
	}
	if fields.TotalFlow && res.TotalFlow != (store.UsageItem{}) {
		lines = append(lines, fmt.Sprintf("  总流量：已用 %s / 总 %s", res.TotalFlow.Used, res.TotalFlow.Total))
	}
	if fields.VoiceUsed && res.Voice != (store.UsageItem{}) {
		lines = append(lines, fmt.Sprintf("  语音：已用 %s / 总 %s", res.Voice.Used, res.Voice.Total))
	}
	if fields.VoiceRemaining && res.Voice != (store.UsageItem{}) {
		lines = append(lines, "  语音剩余："+res.VoiceRemaining)
	}
	if fields.SmsUsed && res.Sms != (store.UsageItem{}) {
		lines = append(lines, fmt.Sprintf("  短信：已用 %s / 总 %s", res.Sms.Used, res.Sms.Total))
	}
	if fields.SmsRemaining && res.Sms != (store.UsageItem{}) {
		lines = append(lines, "  短信剩余："+res.SmsRemaining)
	}
	if fields.QueryTime {
		lines = append(lines, "  查询时间："+res.QueriedAt)
	}
	return lines
}

// FormatOutput 多号结果 → 完整输出文本
func FormatOutput(results []*Result, fieldsOf func(phone string) store.Fields) string {
	var buf bytes.Buffer
	for i, r := range results {
		if r.Err != "" {
			fmt.Fprintf(&buf, "手机号：%s  [失败] %s\n", r.Phone, r.Err)
		} else {
			for _, line := range FormatResultLines(r.Phone, r.Result, fieldsOf(r.Phone)) {
				fmt.Fprintln(&buf, line)
			}
		}
		if i < len(results)-1 {
			fmt.Fprintln(&buf)
		}
	}
	return buf.String()
}

// IsAlert 判断是否命中告警条件（余额低于阈值 / 流量超用）
func IsAlert(res *store.QueryResult, balanceBelow float64, flowPercent int) bool {
	if res == nil {
		return false
	}
	if balanceBelow > 0 && res.BalanceNum < balanceBelow {
		return true
	}
	if flowPercent > 0 && res.FlowUsedPercent >= float64(flowPercent) {
		return true
	}
	return false
}
