package mobile

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

const (
	loginURL = "https://wx.10086.cn/website/bind/bindAccount/new"
	homeURL  = "https://wx.10086.cn/website/spa/main/newHome"
	uaLegacy = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

	queryTimeout = 120 * time.Second
)

// browserMu 全局互斥：同一时刻只允许一个浏览器实例操作登录态，
// 避免多个 Chromium 进程争抢同一 user-data-dir（查询本身是串行的，足够）
var browserMu sync.Mutex

var httpClient = &http.Client{Timeout: 30 * time.Second}

// findBrowserBin 按优先级查找可用的 Chromium 内核浏览器：
//  1. BROWSER_BIN 环境变量（用户显式指定）
//  2. 系统已安装的 Chrome / Edge / Chromium（常见安装路径）
//  3. rod 缓存目录中已下载的浏览器
//  返回空字符串表示都没找到，此时 rod 会自动下载
func findBrowserBin() string {
	if bin := strings.TrimSpace(os.Getenv("BROWSER_BIN")); bin != "" {
		return bin
	}
	for _, candidate := range systemBrowserCandidates() {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, _ := launcher.LookPath(); p != "" {
		return p
	}
	return ""
}

// systemBrowserCandidates 返回各平台常见的 Chromium 内核浏览器安装路径
func systemBrowserCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Chromium\Application\chrome.exe`,
		}
	case "darwin":
		return []string{
			`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`,
			`/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge`,
			`/Applications/Chromium.app/Contents/MacOS/Chromium`,
		}
	default: // linux
		return []string{
			`/usr/bin/google-chrome`,
			`/usr/bin/google-chrome-stable`,
			`/usr/bin/chromium`,
			`/usr/bin/chromium-browser`,
			`/usr/bin/microsoft-edge`,
		}
	}
}

// LaunchBrowser 按项目约定启动 Chromium（复用登录态目录）
func LaunchBrowser(userDataDir string, headless bool) (*rod.Browser, error) {
	// 先清理该目录下残留的浏览器进程（上次异常退出留下的，会锁住 profile 文件）
	killStaleBrowser(userDataDir)

	l := launcher.New().
		// 禁用 leakless 包装进程：PID() 才是真正的 Chrome 主进程 PID，
		// 我们用自己的 PID 文件做残留清理（服务启动时 / 备份导入前）
		Leakless(false).
		UserDataDir(userDataDir).
		Set("user-agent", uaLegacy).
		Set("window-size", "1280,900").
		Set("lang", "zh-CN").
		Set("disable-blink-features", "AutomationControlled")
	if headless {
		// Chrome 132+ 移除了旧版 headless，必须使用 new headless 模式
		l = l.HeadlessNew(true)
	}
	if os.Getenv("BROWSER_NO_SANDBOX") == "1" {
		l = l.NoSandbox(true)
	}
	if bin := findBrowserBin(); bin != "" {
		l = l.Bin(bin)
	}
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	// 记录浏览器 PID（异常退出时下次启动可清理残留进程）
	if pid := l.PID(); pid > 0 {
		_ = os.WriteFile(browserPidFile(userDataDir), []byte(strconv.Itoa(pid)), 0o600)
	}
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("连接浏览器失败: %w", err)
	}
	return browser, nil
}

// CloseBrowser 关闭浏览器并确保整个进程树退出（否则 user-data 目录被锁，影响备份导入等操作）
func CloseBrowser(b *rod.Browser, userDataDir string) {
	if b == nil {
		return
	}
	_ = b.Close() // 发送 CDP 关闭命令（优雅退出）

	pidFile := browserPidFile(userDataDir)
	b2, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b2)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidFile)
		return
	}
	// 给浏览器自然退出的时间（Windows 上 Chrome 退出是异步的）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	// 无条件结束整个进程树：主进程退出不代表 crashpad 等子进程退出
	// （它们继承 user-data 文件句柄，残留会锁住目录；杀已退出进程树无害）
	killProcessTree(pid)
	time.Sleep(800 * time.Millisecond) // 等内核释放文件句柄
	_ = os.Remove(pidFile)
}

// processAlive 见 proc_windows.go / proc_unix.go（平台实现）

// browserPidFile PID 记录文件路径（隐藏在 user-data 目录内）
func browserPidFile(userDataDir string) string {
	return filepath.Join(userDataDir, ".browser.pid")
}

// killStaleBrowser 杀掉记录的残留浏览器进程树并删除记录
func killStaleBrowser(userDataDir string) {
	pidFile := browserPidFile(userDataDir)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidFile)
		return
	}
	killProcessTree(pid)
	// 给浏览器进程一点退出时间，避免后续启动锁冲突
	time.Sleep(800 * time.Millisecond)
	_ = os.Remove(pidFile)
}

// KillStaleBrowsers 清理 dataDir 下所有账号的残留浏览器进程
// （服务启动时 / 备份导入前调用）
func KillStaleBrowsers(dataDir string) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "accounts"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			killStaleBrowser(store.UserDataDir(dataDir, e.Name()))
		}
	}
}

// NewPage 新建标签页并设置视口
func NewPage(b *rod.Browser) (*rod.Page, error) {
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, err
	}
	_ = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: 1280, Height: 900, DeviceScaleFactor: 1, Mobile: false,
	})
	return page, nil
}

// BodyText 读取页面 innerText
func BodyText(p *rod.Page) (string, error) {
	res, err := p.Eval(`() => document.body.innerText`)
	if err != nil {
		return "", err
	}
	return res.Value.Str(), nil
}

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

	browser, err := LaunchBrowser(userDataDir, true)
	if err != nil {
		pr.Err = err.Error()
		return pr
	}
	defer CloseBrowser(browser, userDataDir)

	page, err := NewPage(browser)
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

	pageText, err := BodyText(page)
	if err != nil {
		pr.Err = "读取页面失败: " + err.Error()
		return pr
	}

	// 登录状态检查（与 Python 版一致：页面出现套餐相关字样视为已登录）
	if !strings.Contains(pageText, "动感地带") && !strings.Contains(pageText, "青春卡") && !strings.Contains(pageText, "套餐") {
		pr.Err = fmt.Sprintf("登录状态已过期，请重新登录 %s", phone)
		return pr
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
