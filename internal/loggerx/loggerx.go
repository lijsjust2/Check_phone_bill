package loggerx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry 内存日志条目
type Entry struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Logger 写文件（按天轮转，保留 14 天）+ 控制台 + 内存环形缓冲
type Logger struct {
	mu      sync.Mutex
	dir     string
	file    *os.File
	fileDay string
	ring    []Entry
	max     int
}

// New 创建日志器，日志写入 dataDir/logs
func New(dataDir string) *Logger {
	return &Logger{
		dir: filepath.Join(dataDir, "logs"),
		max: 1000,
	}
}

func (l *Logger) write(level, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	e := Entry{
		Time:  now.Format("2006-01-02 15:04:05"),
		Level: level,
		Msg:   msg,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Printf("%s [%s] %s\n", e.Time, level, msg)
	l.rotateLocked(now)
	if l.file != nil {
		_, _ = fmt.Fprintf(l.file, "%s [%s] %s\n", e.Time, level, msg)
	}
	l.ring = append(l.ring, e)
	if len(l.ring) > l.max {
		l.ring = l.ring[len(l.ring)-l.max:]
	}
}

func (l *Logger) rotateLocked(now time.Time) {
	day := now.Format("2006-01-02")
	if l.file != nil && day == l.fileDay {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
	}
	_ = os.MkdirAll(l.dir, 0o755)
	f, err := os.OpenFile(filepath.Join(l.dir, "app-"+day+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		l.file = nil
		return
	}
	l.file = f
	l.fileDay = day
	l.cleanupLocked()
}

// cleanupLocked 删除 14 天前的日志文件
func (l *Logger) cleanupLocked() {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "app-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 14 {
		for _, n := range names[:len(names)-14] {
			_ = os.Remove(filepath.Join(l.dir, n))
		}
	}
}

// Info 常规信息
func (l *Logger) Info(format string, args ...interface{}) { l.write("INFO", format, args...) }

// Error 错误信息
func (l *Logger) Error(format string, args ...interface{}) { l.write("ERROR", format, args...) }

// Warn 警告信息
func (l *Logger) Warn(format string, args ...interface{}) { l.write("WARN", format, args...) }

// Entries 返回最近 n 条内存日志（新的在前）
func (l *Logger) Entries(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.ring) {
		n = len(l.ring)
	}
	out := make([]Entry, n)
	for i := 0; i < n; i++ {
		out[i] = l.ring[len(l.ring)-1-i]
	}
	return out
}
