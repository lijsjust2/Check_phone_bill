package unicom

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/store"
)

// 网页通道 queryOcsPackageFlowLeftContentRevisedInJune 响应解析。
// 流量数值单位 MB；flowtype：1=通用（全国月包）2=专属（定向）3=其他（区域/免流/闲时等）。

// ParseFlowLeft 余量响应 → store.QueryResult
func ParseFlowLeft(data gjson.Result) *store.QueryResult {
	r := &store.QueryResult{Carrier: carrier.Unicom}
	r.PlanName = data.Get("packageName").Str

	g, s, o := sumFlowUsage(data)

	if g.nonEmpty() {
		r.GeneralFlow = mbItem(g.used, g.total)
	}
	if s.nonEmpty() {
		r.SpecialFlow = mbItem(s.used, s.total)
	}
	if o.nonEmpty() {
		r.RegionalFlow = mbItem(o.used, o.total)
	}

	// 总流量：已用取 allUserFlow（兜底 summary.sum / 分类合计）；总量 = 三类合计
	totalUsed := data.Get("allUserFlow").Float()
	if totalUsed <= 0 {
		totalUsed = data.Get("summary.sum").Float()
	}
	if totalUsed <= 0 {
		totalUsed = g.used + s.used + o.used
	}
	totalAll := g.total + s.total + o.total
	if totalUsed > 0 || totalAll > 0 {
		r.TotalFlow = mbItem(totalUsed, totalAll)
	}

	// 语音：voiceHeadUsed 已用（套外也计入）；voiceSumresource 套内总量（0 = 全套外无免费语音）
	vUsed := data.Get("voiceHeadUsed").Float()
	if vUsed <= 0 {
		vUsed = sumResourceUsed(data.Get("TwResources"))
	}
	vTotal := data.Get("voiceSumresource").Float()
	if vUsed > 0 || vTotal > 0 {
		r.Voice = store.UsageItem{
			Used:     fmtMin(vUsed),
			Total:    fmtMin(vTotal),
			UsedNum:  vUsed,
			TotalNum: vTotal,
			Unit:     "01",
		}
		// 套外语音套餐（vTotal=0、vUsed>0）剩余按 0 分钟计，避免出现空值
		if remain := data.Get("canuseVoiceAll").Float(); remain > 0 {
			r.VoiceRemaining = fmtMin(remain)
		} else {
			left := vTotal - vUsed
			if left < 0 {
				left = 0
			}
			r.VoiceRemaining = fmtMin(left)
		}
	}

	// 短信
	smsUsed := data.Get("smsHeadUsed").Float()
	smsTotal := data.Get("smsSumresource").Float()
	if smsUsed > 0 || smsTotal > 0 {
		r.Sms = store.UsageItem{
			Used:     fmtSms(smsUsed),
			Total:    fmtSms(smsTotal),
			UsedNum:  smsUsed,
			TotalNum: smsTotal,
			Unit:     "02",
		}
		if remain := data.Get("canUseSmsAll").Float(); remain > 0 {
			r.SmsRemaining = fmtSms(remain)
		} else {
			left := smsTotal - smsUsed
			if left < 0 {
				left = 0
			}
			r.SmsRemaining = fmtSms(left)
		}
	}

	// 流量已用百分比（告警用）
	if r.TotalFlow.TotalNum > 0 {
		r.FlowUsedPercent = r.TotalFlow.UsedNum / r.TotalFlow.TotalNum * 100
	}
	return r
}

// ParseBalance accountBalancenew 响应 → 余额/话费（顶层字段，尽力读取）：
//
//	curntbalancecust    当前可用余额（元）
//	totalrealfee        本月实时话费（元，含定向支付；realfeecust 与其同值）
//	realfeecustnew      实时话费（元，不含定向支付部分）
//	allbillfee          本月账单总额（元）
//	monthlyRechargeBill 本月存入/充值（元）
func ParseBalance(r *store.QueryResult, data gjson.Result) {
	d := data.Get("data")
	if !d.Exists() {
		d = data
	}
	if v, ok := parseFee(d.Get("curntbalancecust")); ok {
		r.Balance = fmt.Sprintf("%.2f元", v)
		r.BalanceNum = v
	}
	if v, ok := parseFee(d.Get("totalrealfee")); ok {
		r.RealtimeFee = fmt.Sprintf("%.2f", v)
	} else if v, ok := parseFee(d.Get("realfeecustnew")); ok {
		r.RealtimeFee = fmt.Sprintf("%.2f", v)
	}
	if v, ok := parseFee(d.Get("allbillfee")); ok {
		r.BillTotal = fmt.Sprintf("%.2f", v) // 输出时由 format.go 统一补"元"
	}
}

// ---------- 聚合 ----------

// mbUsage 流量聚合（MB）
type mbUsage struct {
	used  float64
	total float64
}

func (a *mbUsage) add(u mbUsage) {
	a.used += u.used
	a.total += u.total
}

func (a mbUsage) nonEmpty() bool { return a.used > 0 || a.total > 0 }

// sumFlowUsage 流量分类聚合：优先 flowSumList（xusedvalue 已用 + xcanusevalue 剩余 = 总量），
// 缺失时回退 resources[type=flow].details（use/total + flowType 字段）
func sumFlowUsage(data gjson.Result) (g, s, o mbUsage) {
	list := data.Get("flowSumList").Array()
	if len(list) > 0 {
		for _, it := range list {
			used := it.Get("xusedvalue").Float()
			total := used + it.Get("xcanusevalue").Float()
			clsByType(it.Get("flowtype").Str, mbUsage{used: used, total: total}, &g, &s, &o)
		}
		return g, s, o
	}
	for _, res := range data.Get("resources").Array() {
		if res.Get("type").Str != "flow" {
			continue
		}
		for _, d := range res.Get("details").Array() {
			u := mbUsage{used: d.Get("use").Float(), total: d.Get("total").Float()}
			clsByType(d.Get("flowType").Str, u, &g, &s, &o)
		}
	}
	return g, s, o
}

func clsByType(flowType string, u mbUsage, g, s, o *mbUsage) {
	switch flowType {
	case "1":
		g.add(u)
	case "2":
		s.add(u)
	case "3":
		o.add(u)
	}
}

// sumResourceUsed 资源组 userResource 合计（语音兜底：TwResources 各项 userResource 为已用）
func sumResourceUsed(list gjson.Result) float64 {
	var sum float64
	for _, it := range list.Array() {
		sum += it.Get("userResource").Float()
	}
	return sum
}

// ---------- 格式化 ----------

// parseFee 接口余额字段 → 浮点（支持 "23.45"、"-1.20"、"0" 等，无效返回 false）
func parseFee(v gjson.Result) (float64, bool) {
	s := strings.TrimSpace(v.Str)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

const mbPerGB = 1024

// mbItem MB 用量 → UsageItem（GB 数值 + 可读字符串）
func mbItem(used, totalMB float64) store.UsageItem {
	return store.UsageItem{
		Used:     fmtMB(used),
		Total:    fmtMB(totalMB),
		UsedNum:  used / mbPerGB,
		TotalNum: totalMB / mbPerGB,
		Unit:     "04",
	}
}

// fmtMB MB → 可读字符串（≥0.01GB 按 GB 展示且整数省小数，小值按 MB）
func fmtMB(mb float64) string {
	if gb := mb / mbPerGB; gb >= 0.01 {
		if gb == float64(int64(gb)) {
			return fmt.Sprintf("%dGB", int64(gb))
		}
		return trimZero(gb) + "GB"
	}
	if mb == float64(int64(mb)) {
		return fmt.Sprintf("%dMB", int64(mb))
	}
	return fmt.Sprintf("%.2fMB", mb)
}

func fmtMin(v float64) string {
	return strconv.FormatInt(int64(v), 10) + "分钟"
}

func fmtSms(v float64) string {
	return strconv.FormatInt(int64(v), 10) + "条"
}

// trimZero 保留至多两位小数并去掉末尾多余的 0（20.00→"20"，3.06→"3.06"）
func trimZero(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	return s
}
