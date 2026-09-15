package unicom

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// QueryPhone 查询联通账号：网页通道（JUT Cookie 直连 mxx 域，登录态长期有效，无需浏览器）
func QueryPhone(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	phone := acc.Phone
	pr := &carrier.Result{Phone: phone, Raw: map[string]string{}}
	start := time.Now()

	jut := acc.WebToken
	if jut == "" {
		// 历史通道遗留账号（openid/ecs_token）已废弃，需用网页登录重新添加
		pr.Err = "未登录，请重新添加账号并完成网页登录"
		carrier.MarkNotLoggedIn(pr)
		pr.LoginExpired = true
		return pr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, raw, err := QueryWebFlowLeft(ctx, phone, jut, log)
	if errors.Is(err, ErrCookieInvalid) {
		pr.Err = "登录已失效，请重新添加账号并完成网页登录"
		carrier.MarkNotLoggedIn(pr)
		pr.LoginExpired = true
		return pr
	}
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	pr.Raw["queryOcsPackageFlowLeft"] = raw

	pr.Result = ParseFlowLeft(res)
	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")

	// 话费余额（同域同 Cookie，尽力读取，失败不影响余量结果）
	if bal, berr := QueryWebBalance(ctx, phone, jut, log); berr == nil {
		ParseBalance(pr.Result, bal)
	} else if !errors.Is(berr, ErrCookieInvalid) && log != nil {
		log.Info("[%s] 联通话费余额获取失败: %v", phone, berr)
	}

	pr.JSONFn = saveQueryJSON(dataDir, phone, pr)

	if log != nil {
		log.Info("[%s] 联通查询完成（耗时 %s）：余额 %s，套餐 %s，总流量 %s/%s",
			phone, time.Since(start).Round(time.Second), pr.Result.Balance, pr.Result.PlanName, pr.Result.TotalFlow.Used, pr.Result.TotalFlow.Total)
	}
	return pr
}

// saveQueryJSON 原始响应落盘（与移动/电信一致：保留最近 30 次）
func saveQueryJSON(dataDir, phone string, pr *carrier.Result) string {
	dir := filepath.Join(store.AccountsDir(dataDir, phone), "query_results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	out := map[string]interface{}{
		"time":              time.Now().Format("2006-01-02 15:04:05"),
		"phone":             phone,
		"carrier":           carrier.Unicom,
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
