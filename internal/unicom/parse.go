package unicom

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/store"
)

// 资源组 → 展示类别（与开源脚本 resourcesConfig 对齐）
//
//	resources 套餐内流量&流量包 → 通用流量
//	unshared  套餐内流量&流量包(非共享) → 通用流量
//	rzbresources 日租宝 → 区域流量
//	mlresources 免流流量 → 定向流量
//	twresources 套外流量 → 不计入套餐余量
var (
	generalFlowKeys = []string{"resources", "unshared"}
	regionalFlowKey = "rzbresources"
	specialFlowKey  = "mlresources"
)

// 顶层跳过的非资源键
var invalidResourceKeys = map[string]bool{"usepercent": true, "accountbar": true, "code": true, "time": true, "packageName": true, "summary": true}

// ParseFlowLeft queryOcsPackageFlowLeft 响应 → store.QueryResult
//
//	流量原始单位 KB（与移动/电信统一换算 GB）；
//	语音/短信从 type=voice/smslist 等资源项的 details 里尽力读取（分钟/条数）。
func ParseFlowLeft(data gjson.Result) *store.QueryResult {
	r := &store.QueryResult{Carrier: carrier.Unicom}
	r.PlanName = data.Get("packageName").Str

	// 各资源组用量聚合
	var general, regional, special, voice, sms accUsage
	for key, val := range data.Map() {
		if invalidResourceKeys[key] || !val.IsArray() {
			continue
		}
		for _, item := range val.Array() {
			details := item.Get("details")
			if !details.IsArray() {
				continue
			}
			typ := item.Get("type").Str
			switch typ {
			case "voice", "unsharedvoicelist":
				voice.add(details)
			case "smslist", "unsharedsmslist":
				sms.add(details)
			default:
				// 流量类：按顶层键分类
				switch key {
				case regionalFlowKey:
					regional.add(details)
				case specialFlowKey:
					special.add(details)
				default:
					if containsStr(generalFlowKeys, key) {
						general.add(details)
					}
				}
			}
		}
	}

	if general.total > 0 || general.used > 0 {
		r.GeneralFlow = kbItem(general.used, general.total, general.unlimited)
	}
	if regional.total > 0 || regional.used > 0 {
		r.RegionalFlow = kbItem(regional.used, regional.total, regional.unlimited)
	}
	if special.total > 0 || special.used > 0 {
		r.SpecialFlow = kbItem(special.used, special.total, special.unlimited)
	}

	// 总流量：summary.sum 为已用；总量取通用+区域合计（定向流量单独展示）
	totalUsed := max0(data.Get("summary.sum").Float())
	totalAll := general.total + regional.total
	if totalAll <= 0 {
		totalAll = general.used + regional.used
	}
	if totalUsed <= 0 {
		totalUsed = general.used + regional.used + special.used
	}
	if totalUsed > 0 || totalAll > 0 {
		r.TotalFlow = kbItem(totalUsed, totalAll, false)
	}

	if voice.total > 0 || voice.used > 0 {
		r.Voice = store.UsageItem{
			Used:     fmtMin(voice.used),
			Total:    fmtMin(voice.total),
			UsedNum:  voice.used,
			TotalNum: voice.total,
			Unit:     "01",
		}
		if remain := voice.total - voice.used; remain > 0 {
			r.VoiceRemaining = fmtMin(remain)
		}
	}
	if sms.total > 0 || sms.used > 0 {
		r.Sms = store.UsageItem{
			Used:     fmtSms(sms.used),
			Total:    fmtSms(sms.total),
			UsedNum:  sms.used,
			TotalNum: sms.total,
			Unit:     "02",
		}
		if remain := sms.total - sms.used; remain > 0 {
			r.SmsRemaining = fmtSms(remain)
		}
	}

	// 流量已用百分比（告警用）
	if r.TotalFlow.TotalNum > 0 {
		r.FlowUsedPercent = r.TotalFlow.UsedNum / r.TotalFlow.TotalNum * 100
	}
	return r
}

// ParseBalance accountBalancenew 响应 → 余额/实时话费（尽力读取，字段参考 ha_unicom_bill）：
//
//	curntbalancecust 当前余额（元）
//	totalrealfee / realfeecustnew 本月实时话费（元）
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
}

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

// accUsage 明细聚合器（use/total 原始值，主副卡取当前卡已用）
type accUsage struct {
	used      float64
	total     float64
	unlimited bool // 任一明细不限量则整组视为不限量
}

func (a *accUsage) add(details gjson.Result) {
	for _, d := range details.Array() {
		used := d.Get("use").Float()
		// 主副卡：viceCardlist 中 currentLoginFlag=1 的当前卡已用优先
		for _, v := range d.Get("viceCardlist").Array() {
			if v.Get("currentLoginFlag").Str == "1" {
				if cu := v.Get("use").Float(); cu > 0 {
					used = cu
				}
				break
			}
		}
		if used <= 0 {
			used = d.Get("xexceedvalue").Float()
		}
		a.used += max0(used)
		a.total += max0(d.Get("total").Float())
		if d.Get("limited").Str == "1" {
			a.unlimited = true
		}
	}
}

func max0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

const kbPerGB = 1024 * 1024

// kbItem KB 用量 → UsageItem（换算 GB；不限量套餐 Total 显示"不限量"）
func kbItem(used, total float64, unlimited bool) store.UsageItem {
	item := store.UsageItem{
		Used:    fmtGB(used),
		Unit:    "04",
		UsedNum: used / kbPerGB,
	}
	if unlimited {
		item.Total = "不限量"
		item.TotalNum = 0
		item.Unlimited = true
	} else {
		item.Total = fmtGB(total)
		item.TotalNum = total / kbPerGB
	}
	return item
}

// fmtGB KB → "x.xGB"（整数值省略小数，与移动/电信展示风格一致）
func fmtGB(kb float64) string {
	gb := kb / kbPerGB
	if gb == float64(int64(gb)) {
		return strconv.FormatInt(int64(gb), 10) + "GB"
	}
	return trimZero(gb) + "GB"
}

func fmtMin(v float64) string {
	return strconv.FormatInt(int64(v), 10) + "分钟"
}

func fmtSms(v float64) string {
	return strconv.FormatInt(int64(v), 10) + "条"
}

func trimZero(f float64) string {
	// 保留至多两位小数并去掉末尾多余的 0（与移动端展示一致）
	s := strconv.FormatFloat(f, 'f', 2, 64)
	if i := len(s) - 1; i >= 0 && s[i] == '0' {
		s = s[:i]
		if i := len(s) - 1; i >= 0 && s[i] == '.' {
			s = s[:i]
		}
	}
	return s
}
