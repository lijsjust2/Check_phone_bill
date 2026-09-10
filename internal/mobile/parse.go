package mobile

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
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

// PhoneResult 单号查询产物（三网通用类型 carrier.Result 的别名，保持既有引用兼容）
type PhoneResult = carrier.Result
