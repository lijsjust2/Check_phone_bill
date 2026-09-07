package mobile

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/store"
)

var (
	aesKey = []byte("1234123412ABCDEF")
	aesIV  = []byte("ABCDEF1234123412")

	unitMap = map[string]string{"01": "分钟", "02": "条", "03": "MB", "04": "GB"}

	balanceRe1 = regexp.MustCompile(`话费余额\s*\n?\s*(\d+\.\d{2})\s*元`)
	balanceRe2 = regexp.MustCompile(`(\d+\.\d{2})\s*元\s*\n?\s*话费余额`)
)

// TargetAPIs 拦截的目标接口（与 Python 版一致）
var TargetAPIs = []string{
	"getNewMarginInfo", "getMainPlan", "getMarginQueryInfo",
	"getCustBaseInfo",
	"fareBalance", "accountFeeBalanceQuery", "getBillSum",
}

// AESDecrypt AES-128-CBC 解密（中国移动 wx.10086.cn 使用），失败原样返回
func AESDecrypt(dataHex string) string {
	if dataHex == "" || len(dataHex) < 32 {
		return ""
	}
	head := dataHex[:32]
	for _, c := range head {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return dataHex
		}
	}
	data, err := hex.DecodeString(dataHex)
	if err != nil {
		return dataHex
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return dataHex
	}
	if len(data)%aes.BlockSize != 0 {
		return dataHex
	}
	dst := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, aesIV).CryptBlocks(dst, data)
	pad := int(dst[len(dst)-1])
	if pad >= 1 && pad <= 16 {
		dst = dst[:len(dst)-pad]
	}
	return string(dst)
}

// IsHexBody 判断响应是否为 hex 加密体（前 32 位均为 hex 字符）
func IsHexBody(body string) bool {
	if len(body) < 32 {
		return false
	}
	for _, c := range body[:32] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// ExtractBalance 从页面文本中提取余额
func ExtractBalance(pageText string) string {
	if m := balanceRe1.FindStringSubmatch(pageText); m != nil {
		return m[1] + "元"
	}
	if m := balanceRe2.FindStringSubmatch(pageText); m != nil {
		return m[1] + "元"
	}
	return "未知"
}

// SmartDataUnit 根据API返回的unit字段智能换算并带单位（01=分钟 02=条 03=MB 04=GB）
func SmartDataUnit(valStr, unitCode string) string {
	val, err := strconv.ParseFloat(strings.TrimSpace(valStr), 64)
	if err != nil {
		return valStr
	}
	unitName := "GB"
	if u, ok := unitMap[unitCode]; ok {
		unitName = u
	}
	isInt := val == float64(int64(val))
	switch unitCode {
	case "03": // MB -> GB
		gb := val / 1024
		if gb >= 0.01 {
			return fmt.Sprintf("%.2fGB", gb)
		}
		if isInt {
			return fmt.Sprintf("%.0fMB", val)
		}
		return fmt.Sprintf("%.2fMB", val)
	case "04":
		if isInt {
			return fmt.Sprintf("%dGB", int64(val))
		}
		return fmt.Sprintf("%.2fGB", val)
	default:
		if isInt {
			return fmt.Sprintf("%d%s", int64(val), unitName)
		}
		return fmt.Sprintf("%.2f%s", val, unitName)
	}
}

type rawItem struct {
	used   string
	remain string
	total  string
	unit   string
}

func extractItem(r gjson.Result) rawItem {
	return rawItem{
		used:   r.Get("usedNum").Str,
		remain: r.Get("remainNum").Str,
		total:  r.Get("sumNum").Str,
		unit:   r.Get("unit").Str,
	}
}

func toUsageItem(it rawItem) store.UsageItem {
	return store.UsageItem{
		Used:     SmartDataUnit(it.used, it.unit),
		Total:    SmartDataUnit(it.total, it.unit),
		UsedNum:  parseFloat(it.used),
		TotalNum: parseFloat(it.total),
		Unit:     it.unit,
	}
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// ParseResults 由拦截到的各接口响应体（hex 加密或明文 JSON）解析出结构化结果
// bodies: apiName -> 原始响应文本; pageText: 页面文本（余额兜底）
func ParseResults(bodies map[string]string, pageText string) *store.QueryResult {
	parsed := map[string]gjson.Result{}
	for name, body := range bodies {
		if body == "" {
			continue
		}
		text := body
		if IsHexBody(text) {
			text = AESDecrypt(text)
		}
		if !gjson.Valid(text) {
			continue
		}
		parsed[name] = gjson.Parse(text)
	}

	r := &store.QueryResult{}

	// 套餐
	if v, ok := parsed["getMainPlan"]; ok {
		r.PlanName = orDefault(v.Get("object.resultData.curPlanName").Str, "未知")
	}

	// 城市（优先 getCustBaseInfo，回退 getMarginQueryInfo）
	city := ""
	if v, ok := parsed["getCustBaseInfo"]; ok {
		city = v.Get("bean.data.customerAssignment").Str
		if city == "" {
			city = v.Get("bean.data.placeName").Str
		}
	}
	if city == "" {
		if v, ok := parsed["getMarginQueryInfo"]; ok {
			city = v.Get("data.resultData.customerAssignment").Str
		}
	}
	r.City = orDefault(city, "未知")

	// 余额（优先 fareBalance API，回退页面文本）
	balance := "未知"
	if v, ok := parsed["fareBalance"]; ok {
		cur := v.Get("data.realFeeQryRsp.curFeeTotal").Str
		if cur != "" {
			balance = cur + "元"
		}
	}
	if balance == "未知" {
		balance = ExtractBalance(pageText)
	}
	r.Balance = balance
	r.BalanceNum = parseFloat(strings.TrimSuffix(balance, "元"))

	// 实时费用（accountFeeBalanceQuery 或 fareBalance）
	if v, ok := parsed["accountFeeBalanceQuery"]; ok {
		r.RealtimeFee = orDefault(v.Get("data.realFeeQryRsp.realFee").Str, v.Get("data.realFee").Str)
	} else if v, ok := parsed["fareBalance"]; ok {
		r.RealtimeFee = v.Get("data.realFeeQryRsp.realFee").Str
	}

	// 本月账单（getBillSum）
	if v, ok := parsed["getBillSum"]; ok {
		rd := v.Get("object.resultData")
		r.BillCycle = rd.Get("cycleBeginDate").Str + " ~ " + rd.Get("cycleEndDate").Str
		r.BillTotal = rd.Get("toatlBill").Str // API 字段拼写：toatlBill
		r.BillReal = rd.Get("realBillSum").Str
		r.DiscountTotal = rd.Get("costSaveDetails.costSaveTotal").Str
	}

	// 套餐余量（getNewMarginInfo）
	if v, ok := parsed["getNewMarginInfo"]; ok {
		rd := v.Get("data.resultData")
		if flow := rd.Get("planRemianFlowInfo"); flow.Exists() {
			r.GeneralFlow = toUsageItem(extractItem(flow.Get("planRemian")))
			r.SpecialFlow = toUsageItem(extractItem(flow.Get("directionalFlowInfo")))
			r.RegionalFlow = toUsageItem(extractItem(flow.Get("otherRemian")))
			r.TotalFlow = toUsageItem(extractItem(flow.Get("totalInfo")))
		}
		if voice := rd.Get("planRemianVoiceInfo"); voice.Exists() {
			plan := extractItem(voice.Get("planRemian"))
			r.Voice = toUsageItem(plan)
			r.VoiceRemaining = SmartDataUnit(plan.remain, plan.unit)
		}
		if sms := rd.Get("planRemianMSGInfo"); sms.Exists() {
			total := extractItem(sms.Get("totalInfo"))
			r.Sms = toUsageItem(total)
			r.SmsRemaining = SmartDataUnit(total.remain, total.unit)
		}
	}

	// 流量已用百分比（告警用）
	if r.TotalFlow.TotalNum > 0 {
		r.FlowUsedPercent = r.TotalFlow.UsedNum / r.TotalFlow.TotalNum * 100
	}

	return r
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// FormatResultLines 单号结果 → 简洁可读文本（与 Python 版输出一致）
func FormatResultLines(phone string, res *store.QueryResult, fields store.Fields) []string {
	lines := []string{fmt.Sprintf("手机号：%s", phone)}

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
func FormatOutput(results []*PhoneResult, fieldsOf func(phone string) store.Fields) string {
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

// PhoneResult 单号查询产物
type PhoneResult struct {
	Phone  string
	Result *store.QueryResult
	Raw    map[string]string // 原始响应
	JSONFn string            // 落盘文件路径（可选）
	Err    string
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
