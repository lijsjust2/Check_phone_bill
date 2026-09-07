package runner

import (
	"fmt"
	"sync"
	"time"

	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/mobile"
	"chinamobile-monitor/internal/push"
	"chinamobile-monitor/internal/store"
)

// Runner 查询编排：串行查询所有账号、更新状态、记录历史、按设置推送
type Runner struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	dataDir   string
	st        *store.Store
	log       *loggerx.Logger
}

// New 创建查询编排器
func New(dataDir string, st *store.Store, log *loggerx.Logger) *Runner {
	return &Runner{dataDir: dataDir, st: st, log: log}
}

// Status 当前运行状态
func (r *Runner) Status() (running bool, startedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, r.startedAt
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
			r.mu.Unlock()
		}()
		accounts := r.st.ListAccounts()
		results := make([]*mobile.PhoneResult, 0, len(accounts))
		for _, a := range accounts {
			pr := r.queryOneUpdate(a.Phone)
			results = append(results, pr)
		}
		if doPush {
			r.pushResults(results)
		}
	}()
	return nil
}

// QueryOne 立即查询单个账号并更新状态
func (r *Runner) QueryOne(phone string) (*mobile.PhoneResult, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return nil, fmt.Errorf("已有查询在进行中，请稍候")
	}
	r.running = true
	r.startedAt = time.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	if r.st.GetAccount(phone) == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	return r.queryOneUpdate(phone), nil
}

// queryOneUpdate 查询单号并落库（不加锁，由调用方保证串行）
func (r *Runner) queryOneUpdate(phone string) *mobile.PhoneResult {
	pr := mobile.QueryPhone(phone, r.dataDir, r.log)
	now := time.Now()
	if pr.Err != "" {
		r.st.UpdateAccount(phone, func(a *store.Account) {
			a.LastQuery = now
			a.LastOK = false
			a.LastError = pr.Err
		})
		r.st.AddHistory(store.HistoryEntry{
			Time: now.Format("2006-01-02 15:04:05"), Phone: phone, OK: false, Error: pr.Err,
		})
		r.log.Error("[%s] 查询失败: %s", phone, pr.Err)
	} else {
		r.st.UpdateAccount(phone, func(a *store.Account) {
			a.LastQuery = now
			a.LastOK = true
			a.LastError = ""
			a.LastResult = pr.Result
			a.HasLoginState = true
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
func (r *Runner) pushResults(results []*mobile.PhoneResult) {
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

	if cfg.AlertOnly {
		// 仅告警时推送：余额低于阈值 / 流量已用超阈值
		alerts := []*mobile.PhoneResult{}
		for _, pr := range results {
			if pr.Err != "" {
				continue
			}
			if mobile.IsAlert(pr.Result, cfg.AlertBalanceBelow, cfg.AlertFlowPercent) {
				alerts = append(alerts, pr)
			}
		}
		if len(alerts) == 0 {
			r.log.Info("仅告警推送：本次无告警，跳过")
			return
		}
		text := mobile.FormatOutput(alerts, fieldsOf)
		n := push.SendAll(cfg, "【移动监控·告警】", text)
		r.log.Info("告警推送完成（发送渠道数 %d）", n)
		return
	}

	text := mobile.FormatOutput(results, fieldsOf)
	n := push.SendAll(cfg, "【移动套餐用量监控】", text)
	r.log.Info("查询结果推送完成（发送渠道数 %d）", n)
}
