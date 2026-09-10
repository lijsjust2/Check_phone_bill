package telecom

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// QueryPhone 查询电信账号：token 直接查询，失效时用存量服务密码自动重登一次
func QueryPhone(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	phone := acc.Phone
	pr := &carrier.Result{Phone: phone, Raw: map[string]string{}}
	start := time.Now()

	if acc.Token == "" {
		pr.Err = "未登录，请先在面板中添加账号并登录"
		return pr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := QryImportantData(ctx, phone, acc.Token, acc.ProvinceCode, acc.CityCode, log)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	pr.Raw["qryImportantData"] = resp.Raw

	// token 失效 → 存量密码自动重登一次（自愈；androidId 复用已绑定设备）
	if resp.Expired {
		if acc.Password == "" {
			pr.Err = ErrTokenExpired.Error() + "，请重新登录"
			return pr
		}
		if log != nil {
			log.Info("[%s] 电信 token 已失效，尝试自动重新登录", phone)
		}
		state, err := DoLogin(ctx, phone, acc.Password, acc.AndroidID, log)
		if err != nil {
			pr.Err = "自动重登失败: " + err.Error()
			return pr
		}
		newToken, province, city := state.Token, state.ProvinceCode, state.CityCode
		// 回写新登录态（runner 查询成功后应用）
		pr.UpdateAccount = func(a *store.Account) {
			a.Token = newToken
			a.ProvinceCode = province
			a.CityCode = city
		}
		resp, err = QryImportantData(ctx, phone, state.Token, state.ProvinceCode, state.CityCode, log)
		if err != nil {
			pr.Err = err.Error()
			return pr
		}
		pr.Raw["qryImportantData"] = resp.Raw
		if resp.Expired {
			pr.Err = "重新登录后 token 仍无效，请检查账号状态"
			return pr
		}
	}
	if resp.FailMsg != "" {
		pr.Err = resp.FailMsg
		return pr
	}

	pr.Result = ParseImportantData(resp.Data)
	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")
	pr.JSONFn = saveQueryJSON(dataDir, phone, pr)

	if log != nil {
		log.Info("[%s] 电信查询完成（耗时 %s）：余额 %s，总流量 %s/%s",
			phone, time.Since(start).Round(time.Second), pr.Result.Balance, pr.Result.TotalFlow.Used, pr.Result.TotalFlow.Total)
	}
	return pr
}

// ParseImportantData qryImportantData 响应 → store.QueryResult
// 字段映射：totalAmount→总流量 commonFlow→通用流量 specialAmount→定向流量
//
//	voiceInfo→语音 balanceInfo→余额（流量原始单位 KB，统一换算 GB）
func ParseImportantData(data gjson.Result) *store.QueryResult {
	r := &store.QueryResult{Carrier: carrier.Telecom}

	// 余额（元，接口返回浮点字符串）
	if bal := data.Get("balanceInfo.indexBalanceDataInfo.balance"); bal.Exists() {
		if v, err := strconv.ParseFloat(bal.Str, 64); err == nil {
			r.Balance = fmt.Sprintf("%.2f元", v)
			r.BalanceNum = v
		}
	}

	// 流量（KB）；路径缺失时保持零值，输出自动跳过该行
	flow := data.Get("flowInfo")
	if v := flow.Get("totalAmount"); v.Exists() {
		r.TotalFlow = kbItem(v)
	}
	if v := flow.Get("commonFlow"); v.Exists() {
		r.GeneralFlow = kbItem(v)
	}
	if v := flow.Get("specialAmount"); v.Exists() {
		r.SpecialFlow = kbItem(v)
	}

	// 语音（分钟）
	if v := data.Get("voiceInfo.voiceDataInfo"); v.Exists() {
		used, balance, total := v.Get("used").Int(), v.Get("balance").Int(), v.Get("total").Int()
		r.Voice = minItem(used, total)
		r.VoiceRemaining = fmtMin(balance)
	}

	// 归属地（尽力读取，可能缺失）
	r.City = data.Get("provinceName").Str

	// 流量已用百分比（告警用）
	if r.TotalFlow.TotalNum > 0 {
		r.FlowUsedPercent = r.TotalFlow.UsedNum / r.TotalFlow.TotalNum * 100
	}
	return r
}

// kbItem KB 用量 → UsageItem（已换算 GB；Unit 填 "04" 使 flowUsedGB 直接按 GB 取值）
func kbItem(v gjson.Result) store.UsageItem {
	used, balance := v.Get("used").Int(), v.Get("balance").Int()
	total := used + balance
	return store.UsageItem{
		Used:     fmtGB(float64(used)),
		Total:    fmtGB(float64(total)),
		UsedNum:  float64(used) / 1048576,
		TotalNum: float64(total) / 1048576,
		Unit:     "04",
	}
}

// minItem 分钟用量 → UsageItem
func minItem(used, total int64) store.UsageItem {
	return store.UsageItem{
		Used:     fmtMin(used),
		Total:    fmtMin(total),
		UsedNum:  float64(used),
		TotalNum: float64(total),
		Unit:     "01",
	}
}

const kbPerGB = 1024 * 1024

// fmtGB KB → "x.xxGB"（整数值省略小数，与移动端展示风格一致）
func fmtGB(kb float64) string {
	gb := kb / kbPerGB
	if gb == float64(int64(gb)) {
		return fmt.Sprintf("%dGB", int64(gb))
	}
	return strconv.FormatFloat(gb, 'f', 2, 64) + "GB"
}

// fmtMin 分钟数展示
func fmtMin(v int64) string {
	if v == 0 {
		return "0分钟"
	}
	return strconv.FormatInt(v, 10) + "分钟"
}

// saveQueryJSON 原始响应落盘（与移动端一致：保留最近 30 次）
func saveQueryJSON(dataDir, phone string, pr *carrier.Result) string {
	dir := filepath.Join(store.AccountsDir(dataDir, phone), "query_results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	out := map[string]interface{}{
		"time":              time.Now().Format("2006-01-02 15:04:05"),
		"phone":             phone,
		"carrier":           carrier.Telecom,
		"raw_api_responses": pr.Raw,
		"parsed":            pr.Result,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ""
	}
	fn := filepath.Join(dir, "query_"+time.Now().Format("20060102_150405")+".json")
	if err := os.WriteFile(fn, b, 0o644); err != nil {
		return ""
	}
	// 清理旧文件，仅保留最近 30 个
	entries, _ := os.ReadDir(dir)
	if len(entries) > 30 {
		oldest := make([]string, 0, len(entries)-30)
		for i := 0; i < len(entries)-30; i++ {
			oldest = append(oldest, filepath.Join(dir, entries[i].Name()))
		}
		for _, f := range oldest {
			_ = os.Remove(f)
		}
	}
	return fn
}
