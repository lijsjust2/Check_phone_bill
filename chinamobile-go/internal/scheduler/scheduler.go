package scheduler

import (
	"time"

	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/runner"
	"chinamobile-monitor/internal/store"
)

// Scheduler 定时查询（每天在设置的 HH:MM 触发一次）
type Scheduler struct {
	st         *store.Store
	runner     *runner.Runner
	log        *loggerx.Logger
	lastRunDay string
	stop       chan struct{}
}

// New 创建定时器
func New(st *store.Store, r *runner.Runner, log *loggerx.Logger) *Scheduler {
	return &Scheduler{st: st, runner: r, log: log, stop: make(chan struct{})}
}

// Start 启动（每 30 秒检查一次是否到达设定时刻）
func (s *Scheduler) Start() {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-t.C:
				s.check(now)
			}
		}
	}()
	s.log.Info("定时查询已启动，每日 %s 执行", s.st.GetSettings().QueryTime)
}

// Stop 停止
func (s *Scheduler) Stop() { close(s.stop) }

func (s *Scheduler) check(now time.Time) {
	queryTime := s.st.GetSettings().QueryTime
	want, err := time.ParseInLocation("15:04", queryTime, time.Local)
	if err != nil {
		return
	}
	today := now.Format("2006-01-02")
	if s.lastRunDay == today {
		return
	}
	cur := now.Hour()*60 + now.Minute()
	if cur == want.Hour()*60+want.Minute() {
		s.lastRunDay = today
		s.log.Info("到达定时查询时刻 %s，开始查询全部账号", queryTime)
		if err := s.runner.QueryAll(true); err != nil {
			s.log.Error("定时查询启动失败: %v", err)
		}
	}
}
