package mobile

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"chinamobile-monitor/internal/browserx"
	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

const (
	loginURL = "https://wx.10086.cn/website/bind/bindAccount/new"
	homeURL  = "https://wx.10086.cn/website/spa/main/newHome"

	queryTimeout = 120 * time.Second
)

// browserMu 全局互斥：同一时刻只允许一个浏览器实例操作登录态，
// 避免多个 Chromium 进程争抢同一 user-data-dir（查询本身是串行的，足够）
var browserMu sync.Mutex

var httpClient = &http.Client{Timeout: 30 * time.Second}

// EnsureUserDataDir 返回登录态目录；不存在时尝试从 Python 版目录迁移
func EnsureUserDataDir(dataDir, phone string, log *loggerx.Logger) (string, bool, error) {
	dir := store.UserDataDir(dataDir, phone)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir, true, nil
	}
	// 兼容 Python 版登录态：chinamobile_data/<phone>/playwright_user_data
	// （Playwright 与 rod 使用相同的 Chromium profile 格式，可直接迁移）
	for _, cand := range legacyUserdataCandidates(phone) {
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			if err := copyDir(cand, dir); err == nil {
				if log != nil {
					log.Info("已从 Python 版迁移登录态: %s → %s", cand, dir)
				}
				return dir, true, nil
			}
		}
	}
	return dir, false, nil
}

func legacyUserdataCandidates(phone string) []string {
	var out []string
	add := func(base string) {
		if base != "" {
			out = append(out, filepath.Join(base, "chinamobile_data", phone, "playwright_user_data"))
		}
	}
	add(cwd())
	add(exeDir())
	add(parentDir(exeDir())) // 从 chinamobile-go/ 运行时对应仓库根
	return out
}

func cwd() string {
	d, _ := os.Getwd()
	return d
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

func parentDir(d string) string {
	if d == "" {
		return ""
	}
	return filepath.Dir(d)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// QueryPhone 查询单个手机号：无头浏览器打开网厅首页，拦截接口响应解析
func QueryPhone(phone, dataDir string, log *loggerx.Logger) *PhoneResult {
	browserMu.Lock()
	defer browserMu.Unlock()

	start := time.Now()
	pr := &PhoneResult{Phone: phone, Raw: map[string]string{}}

	userDataDir, ok, err := EnsureUserDataDir(dataDir, phone, log)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	if !ok {
		pr.Err = fmt.Sprintf("未找到登录状态，请先登录 %s", phone)
		return pr
	}
	if log != nil {
		log.Info("[%s] 开始查询（复用登录态 %s）", phone, userDataDir)
	}

	browser, err := browserx.LaunchBrowser(userDataDir, true)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	defer browserx.CloseBrowser(browser, userDataDir)

	page, err := browserx.NewPage(browser)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}

	// 接口拦截
	var mu sync.Mutex
	bodies := map[string]string{}
	router := page.HijackRequests()
	_ = router.Add("*", proto.NetworkResourceType(""), func(h *rod.Hijack) {
		u := h.Request.URL().String()
		for _, name := range TargetAPIs {
			if strings.Contains(u, name) {
				if err := h.LoadResponse(httpClient, true); err == nil {
					mu.Lock()
					bodies[name] = h.Response.Body()
					mu.Unlock()
				}
				return
			}
		}
		h.ContinueRequest(&proto.FetchContinueRequest{})
	})
	go func() { router.Run() }()
	defer func() { _ = router.Stop() }()

	// 打开首页
	if err := page.Navigate(homeURL); err != nil {
		pr.Err = "打开网厅失败: " + err.Error()
		return pr
	}
	_ = page.WaitLoad()
	_ = page.WaitIdle(30 * time.Second)
	time.Sleep(5 * time.Second)

	pageText, err := browserx.BodyText(page)
	if err != nil {
		pr.Err = "读取页面失败: " + err.Error()
		return pr
	}

	// 登录状态检查：检测"未登录"特征词（比正向关键词更可靠，
	// 避免因套餐名不含"动感地带/青春卡/套餐"等特定词汇而误判过期）
	notLoggedInKeywords := []string{"请登录", "立即登录", "用户登录", "账号密码登录", "验证码登录"}
	for _, kw := range notLoggedInKeywords {
		if strings.Contains(pageText, kw) {
			pr.Err = fmt.Sprintf("登录状态已过期，请重新登录 %s", phone)
			carrier.MarkNotLoggedIn(pr)
			pr.LoginExpired = true
			return pr
		}
	}

	// getMainPlan 未拦截到时，用页面内 fetch 兜底
	if _, ok := bodies["getMainPlan"]; !ok {
		if res, err := page.Eval(`() => (async () => {
			try {
				const resp = await fetch('https://wx.10086.cn/website/serviceMargin/getMainPlan?t=' + Date.now(), {
					method: 'GET',
					credentials: 'include',
					headers: {
						'Accept': 'application/json, text/plain, */*',
						'X-Requested-With': 'XMLHttpRequest',
					}
				});
				const text = await resp.text();
				return { status: resp.status, ok: resp.ok, body: text };
			} catch(e) {
				return { status: 0, ok: false, body: e.toString() };
			}
		})()`); err == nil && res.Value.Get("ok").Bool() {
			mu.Lock()
			bodies["getMainPlan"] = res.Value.Get("body").Str()
			mu.Unlock()
		}
	}

	mu.Lock()
	for k, v := range bodies {
		pr.Raw[k] = v
	}
	mu.Unlock()

	// 关键接口（getMainPlan/getMarginQueryInfo/getNewMarginInfo）正常应返回加密 hex 数据；
	// 若返回 HTML（如移动"系统优化升级/升级公告"页面），说明登录态已失效或移动侧临时维护，
	// 按登录失效处理（避免解析失败显示"余额 未知"误导用户）
	for _, name := range []string{"getMainPlan", "getMarginQueryInfo", "getNewMarginInfo"} {
		body := pr.Raw[name]
		if body == "" || IsHexBody(body) {
			continue
		}
		if strings.Contains(body, "<html") || strings.Contains(body, "升级公告") ||
			strings.Contains(body, "系统优化升级") || strings.Contains(body, "请登录") {
			pr.Err = fmt.Sprintf("登录状态已过期，请重新登录 %s", phone)
			carrier.MarkNotLoggedIn(pr)
			pr.LoginExpired = true
			return pr
		}
	}

	pr.Result = ParseResults(bodies, pageText)
	pr.Result.QueriedAt = time.Now().Format("2006-01-02 15:04:05")

	// 原始响应落盘（保留最近 30 次）
	pr.JSONFn = saveQueryJSON(dataDir, phone, pr)

	if log != nil {
		log.Info("[%s] 查询完成（耗时 %s）：余额 %s，套餐 %s",
			phone, time.Since(start).Round(time.Second), pr.Result.Balance, pr.Result.PlanName)
	}
	return pr
}

func saveQueryJSON(dataDir, phone string, pr *PhoneResult) string {
	dir := filepath.Join(store.AccountsDir(dataDir, phone), "query_results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	out := map[string]interface{}{
		"time":              time.Now().Format("2006-01-02 15:04:05"),
		"phone":             phone,
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
	// 只保留最近 30 个结果文件
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "query_") && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 30 {
		for _, n := range names[:len(names)-30] {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
	return fn
}
