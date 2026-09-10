package mobile

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"
)

// 与 Python 版相同的 AES-128-CBC 加密（用于生成测试向量，验证解密兼容）
func pythonLikeEncrypt(plain string) string {
	data := []byte(plain)
	pad := aes.BlockSize - len(data)%aes.BlockSize
	for i := 0; i < pad; i++ {
		data = append(data, byte(pad))
	}
	block, _ := aes.NewCipher(aesKey)
	dst := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, aesIV).CryptBlocks(dst, data)
	return hex.EncodeToString(dst)
}

func TestAESDecrypt(t *testing.T) {
	plain := `{"retCode":"000000","data":{"realFeeQryRsp":{"curFeeTotal":"18.30","realFee":"5.20"}}}`
	enc := pythonLikeEncrypt(plain)
	if got := AESDecrypt(enc); got != plain {
		t.Errorf("AES 解密失败:\n got: %s\nwant: %s", got, plain)
	}
	// 明文 JSON 原样返回（长度 > 32 且非 hex 开头）
	plainJSON := `{"object":{"resultData":{"curPlanName":"测试套餐"}}}`
	if got := AESDecrypt(plainJSON); got != plainJSON {
		t.Errorf("明文应原样返回，got %s", got)
	}
	// 非完整 hex 头直接返回原文
	weird := "zzzz" + enc
	if got := AESDecrypt(weird); got != weird {
		t.Errorf("非 hex 开头应原样返回")
	}
}

func TestSmartDataUnit(t *testing.T) {
	cases := []struct {
		val, unit, want string
	}{
		{"1.5", "04", "1.50GB"},
		{"2", "04", "2GB"},
		{"512", "03", "0.50GB"}, // 512MB = 0.50GB
		{"5", "03", "5MB"},      // 5MB < 0.01GB → 显示 5MB
		{"5.5", "03", "5.50MB"}, // 5.5MB < 0.01GB → 显示 5.50MB
		{"30", "01", "30分钟"},
		{"1.5", "01", "1.50分钟"},
		{"10", "02", "10条"},
		{"6.2", "04", "6.20GB"},
	}
	for _, c := range cases {
		if got := SmartDataUnit(c.val, c.unit); got != c.want {
			t.Errorf("SmartDataUnit(%q,%q) = %q, want %q", c.val, c.unit, got, c.want)
		}
	}
}

func TestExtractBalance(t *testing.T) {
	if got := ExtractBalance("话费余额\n18.30元"); got != "18.30元" {
		t.Errorf("got %q", got)
	}
	if got := ExtractBalance("18.30元\n话费余额"); got != "18.30元" {
		t.Errorf("reverse got %q", got)
	}
	if got := ExtractBalance("no balance here"); got != "未知" {
		t.Errorf("fallback got %q", got)
	}
}

func TestParseResults(t *testing.T) {
	bodies := map[string]string{
		"fareBalance":           `{"data":{"realFeeQryRsp":{"curFeeTotal":"18.30","realFee":"5.20","oweFee":"0.00"}}}`,
		"getMainPlan":           pythonLikeEncrypt(`{"object":{"resultData":{"curPlanName":"移动花卡宝藏版"}}}`),
		"getCustBaseInfo":       `{"bean":{"data":{"customerAssignment":"北京"}}}`,
		"getNewMarginInfo":      `{"data":{"resultData":{"planRemianFlowInfo":{"planRemian":{"usedNum":"6.15","remainNum":"23.85","sumNum":"30","unit":"04"},"directionalFlowInfo":{"usedNum":"0","remainNum":"30","sumNum":"30","unit":"04"},"otherRemian":{"usedNum":"1","remainNum":"9","sumNum":"10","unit":"03"},"totalInfo":{"usedNum":"6.65","remainNum":"53.35","sumNum":"60","unit":"04"}},"planRemianVoiceInfo":{"planRemian":{"usedNum":"120","remainNum":"80","sumNum":"200","unit":"01"}},"planRemianMSGInfo":{"totalInfo":{"usedNum":"5","remainNum":"95","sumNum":"100","unit":"02"}}}}}`,
		"getBillSum":            `{"object":{"resultData":{"cycleBeginDate":"2026-09-01","cycleEndDate":"2026-09-30","toatlBill":"39.00","realBillSum":"31.20","costSaveDetails":{"costSaveTotal":"7.80"}}}}`,
		"accountFeeBalanceQuery": `{"data":{"realFeeQryRsp":{"realFee":"5.20"}}}`,
	}
	r := ParseResults(bodies, "")

	if r.Balance != "18.30元" {
		t.Errorf("balance = %q", r.Balance)
	}
	if r.BalanceNum != 18.30 {
		t.Errorf("balanceNum = %v", r.BalanceNum)
	}
	if r.PlanName != "移动花卡宝藏版" {
		t.Errorf("plan = %q", r.PlanName)
	}
	if r.City != "北京" {
		t.Errorf("city = %q", r.City)
	}
	if r.RealtimeFee != "5.20" {
		t.Errorf("realtimeFee = %q", r.RealtimeFee)
	}
	if r.BillTotal != "39.00" || r.BillReal != "31.20" || r.DiscountTotal != "7.80" {
		t.Errorf("bill = %q/%q/%q", r.BillTotal, r.BillReal, r.DiscountTotal)
	}
	if r.BillCycle != "2026-09-01 ~ 2026-09-30" {
		t.Errorf("billCycle = %q", r.BillCycle)
	}
	if r.GeneralFlow.Used != "6.15GB" || r.GeneralFlow.Total != "30GB" {
		t.Errorf("generalFlow = %q / %q", r.GeneralFlow.Used, r.GeneralFlow.Total)
	}
	if r.RegionalFlow.Used != "1MB" { // 1MB < 0.01GB → 显示 1MB
		t.Errorf("regionalFlow(1MB) = %q, want 1MB", r.RegionalFlow.Used)
	}
	if r.TotalFlow.Used != "6.65GB" {
		t.Errorf("totalFlow = %q", r.TotalFlow.Used)
	}
	if r.Voice.Used != "120分钟" || r.Voice.Total != "200分钟" {
		t.Errorf("voice = %q / %q", r.Voice.Used, r.Voice.Total)
	}
	if r.VoiceRemaining != "80分钟" {
		t.Errorf("voiceRemaining = %q", r.VoiceRemaining)
	}
	if r.Sms.Used != "5条" || r.SmsRemaining != "95条" {
		t.Errorf("sms = %q / remain %q", r.Sms.Used, r.SmsRemaining)
	}
	if r.FlowUsedPercent < 11.07 || r.FlowUsedPercent > 11.09 {
		t.Errorf("flowPercent = %v, want ≈11.08", r.FlowUsedPercent)
	}
}
