// Package browserx 共享浏览器基础设施（go-rod 封装）：
// 目前仅移动号使用浏览器（登录/查询）；联通（OpenID）、电信、广电均为纯 HTTP。
// 提供统一的启动/清理逻辑，保证浏览器实例互斥、profile 锁可靠释放。
package browserx

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"

	"chinamobile-monitor/internal/store"
)

const uaLegacy = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

// Mu 全局互斥：同一时刻只允许一个浏览器实例操作登录态，
// 避免多个 Chromium 进程争抢同一 user-data-dir（查询本身是串行的，足够）
var Mu sync.Mutex

// findBrowserBin 按优先级查找可用的 Chromium 内核浏览器：
//  1. BROWSER_BIN 环境变量（用户显式指定）
//  2. 系统已安装的 Chrome / Edge / Chromium（常见安装路径）
//  3. rod 缓存目录中已下载的浏览器
//     返回空字符串表示都没找到，此时 rod 会自动下载
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
	KillStaleBrowser(userDataDir)

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
		l = l.HeadlessNew(true).
			// 容器里 /dev/shm 通常只有 64MB，Chromium 极易崩溃，改用 /tmp
			Set("disable-dev-shm-usage", "").
			Set("disable-gpu", "").
			Set("no-first-run", "")
	} else {
		// rod 的 launcher.New() 默认就带 --headless，不显式删掉的话
		// headless=false 依然启动无头浏览器（用户看不到窗口，登录流程会一直空等）。
		// --no-startup-window 同理：有头模式必须删掉，否则浏览器进程起来了却不创建窗口。
		l = l.Headless(false).
			Delete(flags.Flag("no-startup-window")).
			Set("window-position", "120,80").
			Set("start-maximized", "") // 无窗口管理器的虚拟显示下铺满屏幕，方便远程操作
	}
	// 沙箱：容器/NAS 通常需关闭（root 运行、显式开关、或纯无显示器无头环境）
	if os.Getenv("BROWSER_NO_SANDBOX") == "1" || os.Getuid() == 0 || os.Getenv("DISPLAY") == "" {
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

// browserPidFile PID 记录文件路径（隐藏在 user-data 目录内）
func browserPidFile(userDataDir string) string {
	return filepath.Join(userDataDir, ".browser.pid")
}

// KillStaleBrowser 杀掉记录的残留浏览器进程树并删除记录
func KillStaleBrowser(userDataDir string) {
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
			KillStaleBrowser(store.UserDataDir(dataDir, e.Name()))
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
	// 有头模式：把窗口/标签页提到最前，避免被其他窗口盖住后用户以为"没弹窗"
	_, _ = page.Activate()
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
