package cbn

import (
	"context"
	"encoding/json"
	"errors"
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

// QueryPhone 查询广电账号：Cookie + sessionId 查询话费与套餐。
// 会话过期（701）时提示重新登录（图片验证码需人工输入，无法自动重登）。
func QueryPhone(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	phone := acc.Phone
	pr := &carrier.Result{Phone: phone, Raw: map[string]string{}}
	start := time.Now()

	if acc.Cookie == "" || acc.Token == "" {
		pr.Err = "未登录，请先在面板中添加账号并登录"
		carrier.MarkNotLoggedIn(pr)
		pr.LoginExpired = true
		return pr
	}

	client, err := RestoreClient(acc.Cookie)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	client.SessionID = acc.Token

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 话费余额（主结果）
	balRes, balRaw, err := client.QueryBalanceFee(ctx, phone, log)
	if err != nil {
		pr.Err = err.Error()
		if errors.Is(err, ErrSessionExpired) {
			carrier.MarkNotLoggedIn(pr)
			pr.LoginExpired = true
		}
		return pr
	}
	pr.Raw["qryBalanceFee"] = balRaw

	// 套餐信息（尽力读取，失败不阻断）
	var packRes gjson.Result
	if res, packRaw, perr := client.QueryPhonePackInfo(ctx, phone, log); perr == nil {
		pr.Raw["qryPhonePackInfo"] = packRaw
		packRes = res
	} else if log != nil {
		log.Info("[%s] 广电套餐信息读取失败（不影响话费）: %v", phone, perr)
	}

	// 余量（流量/语音，尽力读取）
	var userRes gjson.Result
	if res, resRaw, rerr := client.QueryUserRes(ctx, phone, log); rerr == nil {
		pr.Raw["qryUserRes"] = resRaw
		userRes = res
	} else if log != nil {
		log.Info("[%s] 广电余量读取失败（不影响话费）: %v", phone, rerr)
	}

	pr.Result = parseCbnResult(balRes, packRes, userRes)

	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")
	pr.JSONFn = saveQueryJSON(dataDir, phone, pr)

	if log != nil {
		log.Info("[%s] 广电查询完成（耗时 %s）：余额 %s",
			phone, time.Since(start).Round(time.Second), pr.Result.Balance)
	}
	return pr
}

// parseCbnResult 广电响应 → store.QueryResult。
// 真实结构（2026-09-10 实测）：
//   - qryBalanceFee: {data:{intfResultBean:{balance:"4911"(分),currealFee:"1670"(分),curCycleId:"202609"}}}
//   - qryPhonePackInfo: {data:{packName:"惠民月卡2.0MAX",packFee:"3900"(分),openDate:"..."}}
//   - qryUserRes: {data:{intfResultBean:{userResList:[{itemTypeCode:"3"流量(KB)/"2"语音(分),
//     highFee:总量, balance:剩余, addupValue:已用}]}}}
func parseCbnResult(bal, pack, userRes gjson.Result) *store.QueryResult {
	r := &store.QueryResult{Carrier: carrier.Cbn}

	// 话费余额（分 → 元）
	if v := bal.Get("data.intfResultBean.balance"); v.Exists() {
		if cents, err := strconv.ParseFloat(v.Str, 64); err == nil {
			r.Balance = fmt.Sprintf("%.2f元", cents/100)
			r.BalanceNum = cents / 100
		}
	}
	// 实时话费（分 → 元；format.go 输出时自动追加“元”后缀）
	if v := bal.Get("data.intfResultBean.currealFee"); v.Exists() {
		if cents, err := strconv.ParseFloat(v.Str, 64); err == nil {
			r.RealtimeFee = fmt.Sprintf("%.2f", cents/100)
		}
	}
	// 账期
	r.BillCycle = bal.Get("data.intfResultBean.curCycleId").Str

	// 套餐名称（含月费展示）
	if name := pack.Get("data.packName").Str; name != "" {
		r.PlanName = name
		if v := pack.Get("data.packFee"); v.Exists() {
			if cents, err := strconv.ParseFloat(v.Str, 64); err == nil && cents > 0 {
				r.PlanName = fmt.Sprintf("%s（月费%.0f元）", name, cents/100)
			}
		}
	}

	// 余量：userResList 各资源汇总（流量 KB / 语音分钟，全部为国内通用资源）
	var flowTotal, flowRemain, flowUsed int64
	var voiceTotal, voiceRemain int64
	for _, item := range userRes.Get("data.intfResultBean.userResList").Array() {
		total := item.Get("highFee").Int()
		remain := item.Get("balance").Int()
		switch item.Get("itemTypeCode").Str {
		case "3": // 流量（KB）
			flowTotal += total
			flowRemain += remain
			flowUsed += item.Get("addupValue").Int()
		case "2": // 语音（分钟）
			voiceTotal += total
			voiceRemain += remain
		}
	}
	if flowTotal > 0 {
		used := flowTotal - flowRemain
		if used < 0 {
			used = flowUsed
		}
		r.GeneralFlow = store.UsageItem{
			Used:     fmtGB(float64(used)),
			Total:    fmtGB(float64(flowTotal)),
			UsedNum:  float64(used) / 1048576,
			TotalNum: float64(flowTotal) / 1048576,
			Unit:     "04",
		}
		r.TotalFlow = r.GeneralFlow
	}
	if voiceTotal > 0 {
		r.Voice = store.UsageItem{
			Used:     strconv.FormatInt(voiceTotal-voiceRemain, 10) + "分钟",
			Total:    strconv.FormatInt(voiceTotal, 10) + "分钟",
			UsedNum:  float64(voiceTotal - voiceRemain),
			TotalNum: float64(voiceTotal),
			Unit:     "01",
		}
		r.VoiceRemaining = strconv.FormatInt(voiceRemain, 10) + "分钟"
	}
	return r
}

// fmtGB KB → "x.xGB"（一位小数；<1GB 显示 MB）
func fmtGB(kb float64) string {
	if kb >= 1048576 {
		return fmt.Sprintf("%.1fGB", kb/1048576)
	}
	return fmt.Sprintf("%.0fMB", kb/1024)
}

// saveQueryJSON 原始响应落盘（保留最近 30 次，供字段映射迭代）
func saveQueryJSON(dataDir, phone string, pr *carrier.Result) string {
	dir := filepath.Join(store.AccountsDir(dataDir, phone), "query_results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	out := map[string]interface{}{
		"time":              time.Now().Format("2006-01-02 15:04:05"),
		"phone":             phone,
		"carrier":           carrier.Cbn,
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
