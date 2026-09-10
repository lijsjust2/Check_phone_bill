package unicom

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// QueryPhone 查询联通账号：微信小程序通道（openid）或 Cookie 直查（失效时自愈一次）
func QueryPhone(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	if acc.OpenID != "" {
		return queryViaWx(acc, dataDir, log)
	}
	return queryViaEcsToken(acc, dataDir, log)
}

// queryViaWx 微信小程序通道（自托管）：getTicket → serviceEntrance → mxx 域查询。
// openid 长期有效，每次查询重新换票，无心跳维持。
func queryViaWx(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	phone := acc.Phone
	pr := &carrier.Result{Phone: phone, Raw: map[string]string{}}
	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ticket, err := WxGetTicket(ctx, acc.OpenID, log)
	if err != nil {
		pr.Err = "微信通道登录态无效: " + err.Error()
		return pr
	}
	cookie, err := WxServiceEntrance(ctx, ticket, log)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	ticketPhone := "wx" + strconv.FormatInt(time.Now().UnixMilli(), 10)

	res, raw, err := QueryWxFlowLeft(ctx, phone, ticket, ticketPhone, cookie, log)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	pr.Raw["wx_queryOcsPackageFlowLeft"] = raw
	pr.Result = ParseFlowLeft(res)
	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")

	// 话费余额（尽力读取，失败不影响余量结果）
	if bal, berr := QueryWxBalance(ctx, phone, ticket, ticketPhone, cookie, log); berr == nil {
		ParseBalance(pr.Result, bal)
	} else if log != nil {
		log.Info("[%s] 联通微信通道话费获取失败: %v", phone, berr)
	}

	pr.JSONFn = saveQueryJSON(dataDir, phone, pr)
	if log != nil {
		log.Info("[%s] 联通微信通道查询完成（耗时 %s）：余额 %s，套餐 %s，总流量 %s/%s",
			phone, time.Since(start).Round(time.Second), pr.Result.Balance, pr.Result.PlanName, pr.Result.TotalFlow.Used, pr.Result.TotalFlow.Total)
	}
	return pr
}

// queryViaEcsToken Cookie 直查（失效时用 token_online 会话维持自愈一次）
func queryViaEcsToken(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	phone := acc.Phone
	pr := &carrier.Result{Phone: phone, Raw: map[string]string{}}
	start := time.Now()

	if acc.Cookie == "" {
		pr.Err = "未登录，请先在面板中添加账号并登录"
		return pr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	effCookie := acc.Cookie // 自愈刷新后的有效 Cookie（话费/套餐接口复用）
	res, raw, err := QueryFlowLeft(ctx, phone, acc.Cookie, log)
	if errors.Is(err, ErrCookieInvalid) {
		// Cookie 失效 → token_online 会话维持换新 Cookie 后重试（自愈）
		if acc.Token == "" || acc.AppID == "" {
			pr.Err = ErrCookieInvalid.Error() + "，请重新登录"
			return pr
		}
		if log != nil {
			log.Info("[%s] 联通 Cookie 已失效，尝试会话维持刷新", phone)
		}
		state, kerr := KeepOnline(ctx, acc.AppID, acc.Token, log)
		if kerr != nil {
			pr.Err = "会话维持失败: " + kerr.Error()
			return pr
		}
		newCookie, newToken := state.Cookie, state.TokenOnline
		effCookie = newCookie
		// 回写新登录态（runner 查询成功后应用）
		pr.UpdateAccount = func(a *store.Account) {
			a.Cookie = newCookie
			if newToken != "" {
				a.Token = newToken
			}
		}
		res, raw, err = QueryFlowLeft(ctx, phone, newCookie, log)
		if errors.Is(err, ErrCookieInvalid) {
			pr.Err = "会话维持后 Cookie 仍无效，请重新登录"
			return pr
		}
	}
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	pr.Raw["queryOcsPackageFlowLeft"] = raw

	pr.Result = ParseFlowLeft(res)
	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")

	// 话费余额（尽力读取：accountBalancenew.htm 同域直连复用 Cookie，失败不影响余量结果）
	if bal, berr := QueryBalance(ctx, phone, effCookie, log); berr == nil {
		ParseBalance(pr.Result, bal)
	} else if log != nil {
		log.Info("[%s] 联通话费余额获取失败: %v", phone, berr)
	}

	// 套餐名称：余量接口缺失时用个人信息接口补充（尽力读取）
	if pr.Result.PlanName == "" {
		if name := QueryMyInfo(ctx, phone, effCookie, log); name != "" {
			pr.Result.PlanName = name
		}
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
