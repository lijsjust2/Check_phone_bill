package runner

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/push"
	"chinamobile-monitor/internal/store"
)

// Runner 查询编排：串行查询所有账号、更新状态、记录历史、按设置推送
type Runner struct {
	mu            sync.Mutex
	running       bool
	startedAt     time.Time
	currentPhone  string // 当前正在查询的号码（用于前端显示"查询中"）
	dataDir       string
	st            *store.Store
	log           *loggerx.Logger
}

// New 创建查询编排器
func New(dataDir string, st *store.Store, log *loggerx.Logger) *Runner {
	return &Runner{dataDir: dataDir, st: st, log: log}
}

// Status 当前运行状态
func (r *Runner) Status() (running bool, startedAt time.Time, currentPhone string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, r.startedAt, r.currentPhone
}

// QueryAll 查询全部账号（后台执行）。push=true 时按设置推送结果
func (r *Runner) QueryAll(doPush bool) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return fmt.Errorf("已有查询在进行中，请稍候")
	}
	r.running = true
	r.startedAt = time.Now()
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.running = false
			r.currentPhone = ""
			r.mu.Unlock()
		}()
		accounts := r.st.ListAccounts()
		okCnt := 0
		start := time.Now()
		r.log.Info("开始查询全部账号（共 %d 个）", len(accounts))
		results := make([]*carrier.Result, 0, len(accounts))
		for _, a := range accounts {
			r.mu.Lock()
			r.currentPhone = a.Phone
			r.mu.Unlock()
			if p := carrier.Get(a.CarrierCode()); p != nil {
				r.log.Info("[%s] 开始查询（%s）", a.Phone, p.Name())
			}
			pr := r.queryOneUpdate(a)
			if pr.Err == "" {
				okCnt++
			}
			results = append(results, pr)
		}
		r.log.Info("全部查询完成：成功 %d/%d（耗时 %s）", okCnt, len(accounts), time.Since(start).Round(time.Second))
		if doPush {
			r.pushResults(results)
		}
	}()
	return nil
}

// QueryOne 立即查询单个账号并更新状态
func (r *Runner) QueryOne(phone string) (*carrier.Result, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil, fmt.Errorf("已有查询在进行中，请稍候")
	}
	r.running = true
	r.startedAt = time.Now()
	r.currentPhone = phone
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.currentPhone = ""
		r.mu.Unlock()
	}()

	acc := r.st.GetAccount(phone)
	if acc == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	return r.queryOneUpdate(acc), nil
}

// queryOneUpdate 查询单号并落库（不加锁，由调用方保证串行）
func (r *Runner) queryOneUpdate(a *store.Account) *carrier.Result {
	phone := a.Phone
	pr := carrier.Get(a.CarrierCode()).Query(a, r.dataDir, r.log)
	now := time.Now()
	if pr.Err != "" {
		r.st.UpdateAccount(phone, func(x *store.Account) {
			x.LastQuery = now
			x.LastOK = false
			x.LastError = pr.Err
			// 查询过程中确认登录态已失效（联通 JUT 过期）时置回未登录
			if pr.UpdateAccount != nil {
				pr.UpdateAccount(x)
			}
		})
		r.st.AddHistory(store.HistoryEntry{
			Time: now.Format("2006-01-02 15:04:05"), Phone: phone, OK: false, Error: pr.Err,
		})
		r.log.Error("[%s] 查询失败: %s", phone, pr.Err)
	} else {
		r.st.UpdateAccount(phone, func(x *store.Account) {
			x.LastQuery = now
			x.LastOK = true
			x.LastError = ""
			x.LastResult = pr.Result
			x.HasLoginState = true
			// 查询过程中自愈了登录态（电信 token 续期）时回写
			if pr.UpdateAccount != nil {
				pr.UpdateAccount(x)
			}
		})
		r.st.AddHistory(store.HistoryEntry{
			Time: now.Format("2006-01-02 15:04:05"), Phone: phone, OK: true,
			Summary: fmt.Sprintf("余额 %s｜套餐 %s｜流量 %s/%s",
				pr.Result.Balance, pr.Result.PlanName, pr.Result.TotalFlow.Used, pr.Result.TotalFlow.Total),
		})
		// 记录当日快照（费用明细页数据源，同日重复查询取最后一次）
		r.st.UpsertDaily(phone, pr.Result)
	}
	return pr
}

// pushResults 按推送设置推送查询结果
func (r *Runner) pushResults(results []*carrier.Result) {
	settings := r.st.GetSettings()
	cfg := settings.Push
	if !push.HasChannel(cfg) {
		r.log.Info("未配置推送渠道，跳过推送")
		return
	}

	fieldsOf := func(phone string) store.Fields {
		if a := r.st.GetAccount(phone); a != nil {
			return a.EffectiveFields(cfg.Fields)
		}
		return cfg.Fields
	}

	// 登录态失效专用推送：独立于 alert_only / 主推送开关，
	// 任何配置了推送渠道的用户都应收到"该重登了"的强提示
	if expired := r.collectExpired(results); len(expired) > 0 {
		carrierName := func(code string) string {
			if p := carrier.Get(code); p != nil {
				return p.Name()
			}
			return code
		}
		var lines []string
		for _, e := range expired {
			ph := e.Phone
			if len(ph) >= 7 {
				ph = ph[:3] + "****" + ph[7:]
			}
			lines = append(lines, fmt.Sprintf("· %s（%s）— %s", ph, carrierName(e.Code), e.Err))
		}
		text := "以下账号登录态已失效，请在面板里重新登录：\n" + strings.Join(lines, "\n")
		n := push.SendAll(cfg, "【登录态失效·请重登】", text)
		r.log.Info("登录态失效推送完成（发送渠道数 %d，账号数 %d）", n, len(expired))
	}

	if cfg.AlertOnly {
		// 仅告警时推送：余额低于阈值 / 流量已用超阈值
		alerts := []*carrier.Result{}
		for _, pr := range results {
			if pr.Err != "" {
				continue
			}
			if carrier.IsAlert(pr.Result, cfg.AlertBalanceBelow, cfg.AlertFlowPercent) {
				alerts = append(alerts, pr)
			}
		}
		if len(alerts) == 0 {
			r.log.Info("仅告警推送：本次无告警，跳过")
			return
		}
		text := carrier.FormatOutputMasked(alerts, fieldsOf)
		n := push.SendAll(cfg, "【话费监控·告警】", text)
		r.log.Info("告警推送完成（发送渠道数 %d）", n)
		return
	}

	// 推送格式：话费总结（低余额清单）+ 全部号码详情
	below := cfg.AlertBalanceBelow
	if below <= 0 {
		below = 10
	}
	text := carrier.FormatSummary(results, below) + "\n以下是手机号详情\n\n" + carrier.FormatOutputMasked(results, fieldsOf)
	n := push.SendAll(cfg, "话费监控", text)
	r.log.Info("查询结果推送完成（发送渠道数 %d）", n)
}

// expiredEntry 登录态失效的账号（用于专用推送）
type expiredEntry struct {
	Phone string
	Code  string
	Err   string
}

// collectExpired 收集本次查询中登录态失效的账号
func (r *Runner) collectExpired(results []*carrier.Result) []expiredEntry {
	out := make([]expiredEntry, 0)
	for _, pr := range results {
		if !pr.LoginExpired {
			continue
		}
		acc := r.st.GetAccount(pr.Phone)
		code := ""
		if acc != nil {
			code = acc.CarrierCode()
		}
		out = append(out, expiredEntry{Phone: pr.Phone, Code: code, Err: pr.Err})
	}
	return out
}
