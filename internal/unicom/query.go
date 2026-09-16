package unicom

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// QueryPhone 查询联通账号（微信小程序 OpenID 通道，ha_unicom_bill 协议）：
// OpenID（长期凭证）→ 现取 ticket → serviceEntrance 会话 Cookie → 余量/话费查询。
// ticket 每次现取现用，偶发失效（999999）时自动重新取票重试一次。
func QueryPhone(acc *store.Account, dataDir string, log *loggerx.Logger) *carrier.Result {
	phone := acc.Phone
	pr := &carrier.Result{Phone: phone, Raw: map[string]string{}}
	start := time.Now()

	openid := acc.OpenID
	if openid == "" {
		pr.Err = "未配置微信小程序 OpenID，请重新添加账号并填写 OpenID"
		carrier.MarkNotLoggedIn(pr)
		pr.LoginExpired = true
		return pr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var (
		res    gjson.Result
		raw    string
		ticket string
		cookie string
		tp     string
		err    error
	)
	for attempt := 1; attempt <= 2; attempt++ {
		ticket, err = getTicket(ctx, openid)
		if err != nil {
			if errors.Is(err, ErrOpenIDInvalid) {
				pr.Err = "OpenID 无效或已失效，请重新抓包获取后重新添加账号"
				carrier.MarkNotLoggedIn(pr)
				pr.LoginExpired = true
			} else {
				pr.Err = err.Error()
			}
			return pr
		}
		cookie = serviceEntrance(ctx, ticket)
		tp = ticketPhone()
		res, raw, err = QueryFlowLeft(ctx, ticket, tp, cookie, log)
		if errors.Is(err, ErrTicketInvalid) && attempt == 1 {
			if log != nil {
				log.Info("[%s] 联通票据失效，自动重新取票重试", phone)
			}
			continue
		}
		break
	}
	if err != nil {
		if errors.Is(err, ErrTicketInvalid) {
			pr.Err = "查询票据已失效，请稍后重试或重新添加账号"
		} else {
			pr.Err = err.Error()
		}
		return pr
	}
	pr.Raw["queryOcsPackageFlowLeft"] = raw

	pr.Result = ParseFlowLeft(res)
	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")

	// 话费余额（同 ticket 同 Cookie，尽力读取，失败不影响余量结果）
	if bal, berr := QueryBalance(ctx, ticket, tp, cookie, log); berr == nil {
		ParseBalance(pr.Result, bal)
	} else if !errors.Is(berr, ErrTicketInvalid) && log != nil {
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
