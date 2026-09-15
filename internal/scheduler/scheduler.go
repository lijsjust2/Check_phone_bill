package scheduler

import (
	"strconv"
	"strings"
	"time"

	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/runner"
	"chinamobile-monitor/internal/store"
)

// Scheduler 定时查询（每日在设置的若干 HH:MM 时刻各触发一次）。
// 多个时间用英文逗号分隔，如 "08:00,20:00"，可兼作保活（每天多查几次让服务端 session 续期）。
type Scheduler struct {
	st      *store.Store
	runner  *runner.Runner
	log     *loggerx.Logger
	lastRun string // "YYYY-MM-DD_HH:MM"，每个时间槽独立计 key，避免 08:00 跑两次
	stop    chan struct{}
}

// New 创建定时器
func New(st *store.Store, r *runner.Runner, log *loggerx.Logger) *Scheduler {
	return &Scheduler{st: st, runner: r, log: log, stop: make(chan struct{})}
}

// Start 启动（每 30 秒检查一次是否到达任一设定时刻）
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
	s.log.Info("定时查询已启动，每日 %s 执行", formatSchedule(s.st.GetSettings().QueryTime))
}

// Stop 停止
func (s *Scheduler) Stop() { close(s.stop) }

// parseSchedule 解析 "HH:MM" 或 "HH:MM,HH:MM,..." 为当天 minutes-since-midnight 列表
func parseSchedule(s string) []int {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		t, err := time.ParseInLocation("15:04", p, time.Local)
		if err != nil {
			continue
		}
		out = append(out, t.Hour()*60+t.Minute())
	}
	return out
}

func formatSchedule(s string) string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return "08:00"
	}
	return strings.Join(out, ",")
}

func (s *Scheduler) check(now time.Time) {
	cur := now.Hour()*60 + now.Minute()
	slots := parseSchedule(s.st.GetSettings().QueryTime)
	if len(slots) == 0 {
		return
	}
	for _, want := range slots {
		if cur != want {
			continue
		}
		key := now.Format("2006-01-02") + "_" + strconv.Itoa(want/60) + ":" + strconv.Itoa(want%60)
		if s.lastRun == key {
			continue
		}
		s.lastRun = key
		s.log.Info("到达定时查询时刻 %02d:%02d，开始查询全部账号", want/60, want%60)
		if err := s.runner.QueryAll(true); err != nil {
			s.log.Error("定时查询启动失败: %v", err)
		}
	}
}
