package carrier

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"chinamobile-monitor/internal/store"
)

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// zeroUsage 判断"总量/剩余"这类带单位字符串是否为零（"0分钟" / "0MB" / "0"）
func zeroUsage(s string) bool {
	if s == "" {
		return true
	}
	f, err := strconv.ParseFloat(strings.TrimRight(s, "分钟条GBMKB字节个 "), 64)
	return err == nil && f == 0
}

// usageLine 已用/总量展示：套餐内不含该项（总量为 0）时只显示已用，
// 避免出现"语音：已用 16分钟 / 总 0分钟"这种自相矛盾的文案
func usageLine(name string, u store.UsageItem) string {
	if zeroUsage(u.Total) {
		return fmt.Sprintf("  %s：已用 %s", name, u.Used)
	}
	return fmt.Sprintf("  %s：已用 %s / 总 %s", name, u.Used, u.Total)
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
		lines = append(lines, usageLine("语音", res.Voice))
	}
	// 套餐内不含语音（总量为 0）时"语音剩余 0 分钟"是噪声，不展示
	if fields.VoiceRemaining && res.Voice != (store.UsageItem{}) &&
		!(zeroUsage(res.Voice.Total) && zeroUsage(res.VoiceRemaining)) {
		lines = append(lines, "  语音剩余："+res.VoiceRemaining)
	}
	if fields.SmsUsed && res.Sms != (store.UsageItem{}) {
		lines = append(lines, usageLine("短信", res.Sms))
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

// FormatOutputMasked 推送用：手机号脱敏后的输出（微信等外部渠道）
func FormatOutputMasked(results []*Result, fieldsOf func(phone string) store.Fields) string {
	var buf bytes.Buffer
	for i, r := range results {
		if r.Err != "" {
			fmt.Fprintf(&buf, "手机号：%s  [失败] %s\n", MaskPhone(r.Phone), r.Err)
		} else {
			for _, line := range FormatResultLines(MaskPhone(r.Phone), r.Result, fieldsOf(r.Phone)) {
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

// MaskPhone 手机号脱敏（保留前 3 后 4）
func MaskPhone(phone string) string {
	if len(phone) != 11 {
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}

// FormatSummary 推送开头的话费总结段：低于阈值余额的号码按列表序号列出
func FormatSummary(results []*Result, below float64) string {
	low := make([]*Result, 0, len(results))
	for _, r := range results {
		if r.Err == "" && r.Result != nil && r.Result.BalanceNum < below {
			low = append(low, r)
		}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "话费总结：\n低于%.0f元的手机号，%d个\n", below, len(low))
	for i, r := range low {
		fmt.Fprintf(&buf, "%d、%s：当前话费%s\n", i+1, MaskPhone(r.Phone), orDefault(r.Result.Balance, "未知"))
	}
	return buf.String()
}
