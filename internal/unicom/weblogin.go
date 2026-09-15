package unicom

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// 联通网页登录：弹出真浏览器窗口打开联通网厅（未登录自动跳 uac 统一登录页），
// 用户在浏览器内自行完成登录（密码登录 / 随机密码登录任选，滑块与短信验证码
// 均在真实浏览器内完成，域名与滑块签发域一致故无票据被拒问题）。
//
// 面板每 2 秒扫描浏览器 Cookie 中的 JUT；拿到 JUT 后并不立即判定成功，而是
// 直接调用余量查询接口做一次真实校验（未登录时联通返回纯文本 999999），
// 只有返回 code=0000 才认为登录完成——避免把登录页的游客态 JUT 误判为已登录。
//
// JUT 落在 .10010.com 域，实测 1 小时以上仍可用；此后查询纯 HTTP 直连 mxx 域，
// 只带 JUT 一个 Cookie 即可（无需 SHAREJSESSIONID / acw_tc / piw 等），不再开浏览器。

const (
	webLoginTimeout = 10 * time.Minute // 整会话上限（覆盖滑块 + 短信到达 + 输入）
	webMonitorTick  = 2 * time.Second  // JUT Cookie 轮询间隔
	webValidateWait = 12 * time.Second // 单次登录态校验超时
)

// WebLoginFlow 一次网页登录会话（有头浏览器，实现 carrier.LoginSession 与 LoginStateSaver）
type WebLoginFlow struct {
	mu    sync.Mutex
	Phone string
	Stage string
	Msg   string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	log    *loggerx.Logger

	jut string // 登录成功后的 JUT Cookie（SaveLogin 回写 Account.WebToken）
}

// StartLogin 启动网页登录会话（headless 参数忽略：必须弹真窗口让用户完成滑块）
func StartLogin(phone, dataDir string, headless bool, log *loggerx.Logger) (*WebLoginFlow, error) {
	if !store.ValidPhone(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	userDataDir := store.UserDataDir(dataDir, phone)
	if _, err := os.Stat(userDataDir); err == nil {
		log.Info("[%s] 检测到已有登录状态，将覆盖", phone)
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), webLoginTimeout)
	f := &WebLoginFlow{
		Phone:  phone,
		Stage:  carrier.StageStarting,
		Msg:    "正在启动浏览器窗口...",
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		log:    log,
	}
	go f.run(userDataDir)
	return f, nil
}

func (f *WebLoginFlow) run(userDataDir string) {
	defer close(f.done)
	defer f.cancel()

	// 联通查询走 HTTP+JUT 不占浏览器；登录浏览器与移动模块的 profile 不同目录，
	// 不抢 browserx.Mu（避免阻塞移动账号的定时查询）
	browser, err := browserx.LaunchBrowser(userDataDir, false)
	if err != nil {
		f.setStage(carrier.StageError, err.Error())
		return
	}
	defer browserx.CloseBrowser(browser, userDataDir)

	page, err := browserx.NewPage(browser)
	if err != nil {
		f.setStage(carrier.StageError, err.Error())
		return
	}
	// 登录页风控会读 UA，用完整 Chrome UA（browserx 默认 UA 过旧可能被判异常）
	_ = page.SetUserAgent(&proto.NetworkSetUserAgentOverride{UserAgent: webUA})

	f.setStage(carrier.StagePageLoading, "正在打开联通网上营业厅...")
	f.log.Info("[%s] 打开网厅入口（未登录自动跳统一登录页）", f.Phone)
	if !navigateAny(page, append([]string{WebHallURL}, webHallFallbacks...)) {
		f.setStage(carrier.StageError, "打开登录页失败")
		return
	}
	_ = page.WaitLoad()
	_ = page.WaitIdle(30 * time.Second)
	time.Sleep(2 * time.Second)

	// 尽力自动填手机号（找不到元素则由用户手动输入，不影响流程）
	f.autoFillPhone(page)

	f.setStage(carrier.StageWaitingUser, "")
	f.log.Info("[%s] 等待用户在浏览器窗口中完成登录（监测 JUT Cookie）", f.Phone)

	// 轮询监测 JUT Cookie（登录成功后写入 .10010.com 域）
	tick := time.NewTicker(webMonitorTick)
	defer tick.Stop()
	lastChecked := "" // 已校验失败的 JUT，避免同一个值反复请求
	for {
		select {
		case <-f.ctx.Done():
			f.setStage(carrier.StageClosed, "已取消或超时（未检测到登录完成）")
			return
		case <-tick.C:
			jut, ok := lookupJUT(browser)
			if !ok {
				// 浏览器被用户关闭
				f.setStage(carrier.StageClosed, "浏览器窗口已关闭，登录未完成")
				return
			}
			if jut == "" || jut == lastChecked {
				continue
			}
			// 拿到 JUT 后做一次真实查询校验：未登录时联通返回 999999
			f.setStage(carrier.StageWaitingUser, "已获取登录票据，正在校验...")
			vctx, vcancel := context.WithTimeout(f.ctx, webValidateWait)
			verr := ValidJUT(vctx, jut)
			vcancel()
			if verr != nil {
				// 只有"票据无效"才跳过该 JUT；网络类错误下一轮重试，避免误判
				if errors.Is(verr, ErrCookieInvalid) {
					lastChecked = jut
					f.log.Info("[%s] 登录票据无效（游客态），继续等待用户完成登录", f.Phone)
				} else {
					f.log.Info("[%s] 登录态校验失败（%v），稍后重试", f.Phone, verr)
				}
				f.setStage(carrier.StageWaitingUser, "")
				continue
			}
			f.mu.Lock()
			f.jut = jut
			f.mu.Unlock()
			f.setStage(carrier.StageSuccess, "")
			f.log.Info("[%s] JUT Cookie 校验通过，登录成功", f.Phone)
			return
		}
	}
}

// navigateAny 依次尝试入口页，任一打开成功即返回 true（网厅入口偶尔调整，多备几个）
func navigateAny(page *rod.Page, urls []string) bool {
	for _, u := range urls {
		if u == "" {
			continue
		}
		if err := page.Navigate(u); err == nil {
			return true
		}
	}
	return false
}

// autoFillPhone 尽力把手机号填入登录页账号输入框（uac 登录页元素 id 可能调整，失败静默）
func (f *WebLoginFlow) autoFillPhone(page *rod.Page) {
	_, _ = page.Eval(`(phone) => {
		const sel = ['#userName', '#phone', '#mobile', '#loginName', '#uName',
			'input[type=tel]', 'input[name=userName]', 'input[name=phone]',
			'input[placeholder*="手机"]', 'input[placeholder*="账号"]', 'input[placeholder*="用户名"]'];
		for (const s of sel) {
			const el = document.querySelector(s);
			if (el && el.offsetParent !== null) {
				el.focus();
				el.value = phone;
				el.dispatchEvent(new Event('input', {bubbles: true}));
				el.dispatchEvent(new Event('change', {bubbles: true}));
				return s;
			}
		}
		return '';
	}`, f.Phone)
}

// lookupJUT 浏览器全部 Cookie 中查找 JUT；第二个返回值 false 表示浏览器已关闭
func lookupJUT(browser *rod.Browser) (string, bool) {
	cookies, err := browser.GetCookies()
	if err != nil {
		return "", false
	}
	for _, c := range cookies {
		if c.Name == "JUT" && strings.TrimSpace(c.Value) != "" {
			return strings.TrimSpace(c.Value), true
		}
	}
	return "", true
}

// ---------- carrier.LoginSession ----------

// SubmitCode 网页登录验证码在浏览器窗口内输入，面板无需提交（返回错误提示）
func (f *WebLoginFlow) SubmitCode(code string) error {
	return fmt.Errorf("联通登录请在弹出的浏览器窗口中完成，无需在此输入验证码")
}

func (f *WebLoginFlow) Cancel() { f.cancel() }

func (f *WebLoginFlow) Done() <-chan struct{} { return f.done }

func (f *WebLoginFlow) Status() (stage, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Stage, f.Msg
}

func (f *WebLoginFlow) CodeSubmitted() bool { return false }

// ---------- carrier.LoginStateSaver ----------

// SaveLogin 登录成功后由 Web 层调用：JUT 回写 Account.WebToken
func (f *WebLoginFlow) SaveLogin(a *store.Account) {
	f.mu.Lock()
	jut := f.jut
	f.mu.Unlock()
	if jut == "" {
		return
	}
	a.WebToken = jut
}

// ---------- 内部 ----------

func (f *WebLoginFlow) setStage(stage, msg string) {
	f.mu.Lock()
	f.Stage = stage
	f.Msg = msg
	f.mu.Unlock()
}
