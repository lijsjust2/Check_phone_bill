package mobile

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"

	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// 登录流程阶段（Web 端点轮询展示，CLI 端打印）
const (
	StageStarting    = "starting"     // 启动浏览器
	StagePageLoading = "page_loading" // 打开登录页
	StageCodeSending = "code_sending" // 勾选协议 / 填手机号 / 发送验证码
	StageWaitingCode = "waiting_code" // 等待用户输入短信验证码
	StageSendFailed  = "send_failed"  // 验证码发送失败（仍可手动输入）
	StageSubmitting  = "submitting"   // 已填入验证码，等待登录完成
	StageSuccess     = "success"
	StageError       = "error"
	StageClosed      = "closed" // 浏览器被关闭 / 用户取消
)

var stageText = map[string]string{
	StageStarting:    "正在启动浏览器...",
	StagePageLoading: "正在打开登录页...",
	StageCodeSending: "正在填写手机号并发送验证码...",
	StageWaitingCode: "验证码已发送，请输入收到的短信验证码",
	StageSendFailed:  "验证码发送失败",
	StageSubmitting:  "验证码已提交，等待登录完成...",
	StageSuccess:     "登录成功",
	StageError:       "发生错误",
	StageClosed:      "浏览器已关闭",
}

// StageText 阶段中文说明
func StageText(stage string) string {
	if t, ok := stageText[stage]; ok {
		return t
	}
	return stage
}

// LoginFlow 一次登录会话（CLI 有头 / Web 无头共用），
// 端口自 Python 版 login()：自动勾协议、填手机号、点验证码、监测登录状态
type LoginFlow struct {
	mu     sync.Mutex
	Phone  string
	Stage  string
	Msg    string

	codeSubmitted bool // 是否已提交过验证码（浏览器关闭时判断登录态是否可能已保存）

	ctx    context.Context
	cancel context.CancelFunc
	codeCh chan string
	done   chan struct{}
	log    *loggerx.Logger
}

// StartLogin 启动登录流程。headless=true 用于 Web 面板，false 用于本地 CLI（弹出浏览器窗口）
func StartLogin(phone, dataDir string, headless bool, log *loggerx.Logger) (*LoginFlow, error) {
	if !validPhone11(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	userDataDir := loginUserDataDir(dataDir, phone)
	if _, err := os.Stat(userDataDir); err == nil {
		log.Info("[%s] 检测到已有登录状态，将覆盖", phone)
	}
	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	f := &LoginFlow{
		Phone:  phone,
		Stage:  StageStarting,
		ctx:    ctx,
		cancel: cancel,
		codeCh: make(chan string, 4),
		done:   make(chan struct{}),
		log:    log,
	}
	go f.run(userDataDir, headless)
	return f, nil
}

func validPhone11(p string) bool {
	if len(p) != 11 {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func loginUserDataDir(dataDir, phone string) string {
	// 与查询共用同一登录态目录
	return store.UserDataDir(dataDir, phone)
}

func (f *LoginFlow) setStage(stage, msg string) {
	f.mu.Lock()
	f.Stage = stage
	f.Msg = msg
	f.mu.Unlock()
}

// setMsg 仅更新提示信息（阶段不变，用于倒计时等实时反馈）
func (f *LoginFlow) setMsg(msg string) {
	f.mu.Lock()
	f.Msg = msg
	f.mu.Unlock()
}

// Status 读取当前阶段
func (f *LoginFlow) Status() (stage, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Stage, f.Msg
}

// CodeSubmitted 是否已提交过验证码（浏览器关闭时用于提示登录态是否可能已保存）
func (f *LoginFlow) CodeSubmitted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codeSubmitted
}

// SubmitCode 提交短信验证码（由 Web API 或 CLI stdin 调用）
func (f *LoginFlow) SubmitCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("验证码不能为空")
	}
	select {
	case <-f.ctx.Done():
		return fmt.Errorf("登录会话已结束")
	case f.codeCh <- code:
		return nil
	}
}

// Cancel 取消登录并关闭浏览器
func (f *LoginFlow) Cancel() {
	f.cancel()
}

// Done 流程结束信号
func (f *LoginFlow) Done() <-chan struct{} { return f.done }

func (f *LoginFlow) run(userDataDir string, headless bool) {
	defer close(f.done)
	defer f.cancel()

	browserMu.Lock()
	defer browserMu.Unlock()

	browser, err := LaunchBrowser(userDataDir, headless)
	if err != nil {
		f.setStage(StageError, err.Error())
		return
	}
	defer CloseBrowser(browser, userDataDir)

	page, err := NewPage(browser)
	if err != nil {
		f.setStage(StageError, err.Error())
		return
	}

	// 监测登录状态（每 3 秒检查一次页面，检测到成功关键词或跳转到首页立即返回）
	monitorCh := make(chan bool, 1) // true = 检测到登录成功
	browserClosed := make(chan struct{})
	go func() {
		for {
			select {
			case <-f.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			text, err := BodyText(page)
			if err != nil {
				// 浏览器已关闭
				close(browserClosed)
				return
			}
			if strings.Contains(text, "退出") || strings.Contains(text, "余额") ||
				strings.Contains(text, "套餐") || strings.Contains(text, "我的") {
				monitorCh <- true
				return
			}
			// 登录成功后页面通常跳转到新版首页
			if info, ierr := page.Info(); ierr == nil && info != nil && strings.Contains(info.URL, "newHome") {
				monitorCh <- true
				return
			}
		}
	}()

	f.setStage(StagePageLoading, "")
	f.log.Info("[%s] 打开登录页面", f.Phone)
	if err := page.Navigate(loginURL); err != nil {
		f.setStage(StageError, "打开登录页失败: "+err.Error())
		return
	}
	_ = page.WaitLoad()
	_ = page.WaitIdle(30 * time.Second)
	time.Sleep(2 * time.Second)

	// 勾选隐私政策
	f.log.Info("[%s] 勾选隐私政策", f.Phone)
	if _, err := page.Eval(`() => {
		const el = document.querySelector('.checkCtl.noCheck') || document.querySelector('.checkCtl');
		if (el) { el.click(); return 'clicked'; }
		return 'NOT_FOUND';
	}`); err != nil {
		f.setStage(StageError, "勾选隐私政策失败: "+err.Error())
		return
	}
	time.Sleep(1 * time.Second)

	// 填写手机号
	f.setStage(StageCodeSending, "")
	f.log.Info("[%s] 填写手机号", f.Phone)
	if _, err := page.Eval(`(phone) => {
		const inp = document.querySelector('#phone') || document.querySelector('input[type="tel"]');
		if (inp) {
			inp.value = phone;
			inp.dispatchEvent(new Event('input', {bubbles: true}));
			return 'filled';
		}
		return 'NOT_FOUND';
	}`, f.Phone); err != nil {
		f.setStage(StageError, "填写手机号失败: "+err.Error())
		return
	}
	time.Sleep(1 * time.Second)

	// 点击发送验证码
	f.log.Info("[%s] 发送验证码", f.Phone)
	if _, err := page.Eval(`() => {
		const candidates = [...document.querySelectorAll('button, a, span, div')].filter(e => e.offsetWidth > 0);
		for (const e of candidates) {
			const t = (e.innerText || e.textContent || '').trim();
			if ((t === '获取验证码' || t === '发送验证码') && e.children.length < 3) {
				e.click();
				return 'clicked:' + e.tagName + ':' + t;
			}
		}
		return 'NOT_FOUND';
	}`); err != nil {
		f.setStage(StageError, "点击发送验证码失败: "+err.Error())
		return
	}
	time.Sleep(3 * time.Second)

	// 检查发送状态（#code 的 placeholder 会提示频繁/过多/失败等）
	sendErr := ""
	if res, err := page.Eval(`() => {
		const inp = document.querySelector('#code');
		if (!inp) return '';
		const ph = inp.placeholder || '';
		if (ph && (ph.includes('频繁') || ph.includes('过多') || ph.includes('稍后') || ph.includes('失败'))) return ph;
		return '';
	}`); err == nil {
		sendErr = res.Value.Str()
	}

	if sendErr != "" {
		f.setStage(StageSendFailed, "发送失败："+sendErr+"。可稍后重试，或直接输入收到的验证码")
		f.log.Warn("[%s] 验证码发送失败: %s", f.Phone, sendErr)
	} else {
		f.setStage(StageWaitingCode, "")
		f.log.Info("[%s] 验证码已发送，等待输入", f.Phone)
	}

	// 主循环：监测登录成功 / 浏览器关闭 / 用户提交验证码
	for {
		select {
		case <-f.ctx.Done():
			f.setStage(StageClosed, "已取消")
			return
		case <-browserClosed:
			// 兜底：关闭瞬间监测器可能已检测到登录成功（select 竞态）
			select {
			case <-monitorCh:
				f.setStage(StageSuccess, "")
				f.log.Info("[%s] 登录成功", f.Phone)
			default:
				f.setStage(StageClosed, "尚未提交验证码，请重新登录")
			}
			return
		case ok := <-monitorCh:
			if ok {
				f.setStage(StageSuccess, "")
				f.log.Info("[%s] 登录成功", f.Phone)
			}
			return
		case code := <-f.codeCh:
			if s, _ := f.Status(); s == StageSubmitting || s == StageSuccess {
				continue
			}
			f.mu.Lock()
			f.codeSubmitted = true
			f.mu.Unlock()
			f.setStage(StageSubmitting, "")
			f.log.Info("[%s] 收到验证码，填入浏览器", f.Phone)
			res, err := page.Eval(`(code) => {
				const inp = document.querySelector('#code') || document.querySelector('input[placeholder*="验证码"]');
				if (inp) {
					try { inp.removeAttribute('readonly'); } catch(e) {}
					inp.value = code;
					inp.dispatchEvent(new Event('input', {bubbles: true}));
				}
				const btn = document.querySelector('#loginBtn') || document.querySelector('button[type="submit"]');
				if (btn) {
					btn.click();
					return 'done';
				}
				return 'no-btn';
			}`, code)
			if err != nil {
				f.setStage(StageError, "填入验证码失败: "+err.Error())
				return
			}
			if res.Value.Str() == "no-btn" {
				f.setStage(StageWaitingCode, "未找到登录按钮，请在弹出的浏览器窗口中手动点击登录")
				f.log.Warn("[%s] 未找到登录按钮，等待手动操作", f.Phone)
				continue
			}
			// 等待登录完成（返回 false=流程已终结；true=可重新输入验证码）
			if !f.waitLoginResult(page, monitorCh, browserClosed) {
				return
			}
		}
	}
}

// waitLoginResult 提交验证码后等待登录结果：
// 每 2 秒检查一次页面错误提示（验证码错误/过期等），发现错误立即返回重新输入；
// 期间在提示信息中显示剩余等待秒数；最长等待 10 秒。
// 返回 false 表示流程已终结（成功/取消/浏览器关闭），true 表示回到等待验证码输入。
func (f *LoginFlow) waitLoginResult(page *rod.Page, monitorCh chan bool, browserClosed chan struct{}) bool {
	const maxWait = 10 * time.Second
	deadline := time.Now().Add(maxWait)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	f.setMsg("正在等待登录完成（最长 10 秒）")
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			f.setStage(StageWaitingCode, "未检测到登录成功，请确认验证码是否正确后重新输入")
			return true
		}
		select {
		case <-f.ctx.Done():
			f.setStage(StageClosed, "已取消")
			return false
		case <-browserClosed:
			// 兜底：关闭瞬间监测器可能已检测到登录成功（select 竞态）
			select {
			case <-monitorCh:
				f.setStage(StageSuccess, "")
				f.log.Info("[%s] 登录成功", f.Phone)
			default:
				// 验证码已提交，Cookie 通常已落盘：登录态大概率已保存，引导用户查询验证
				f.setStage(StageClosed, "验证码已提交，若验证码正确，登录状态已自动保存，可点击「查询验证」确认")
			}
			return false
		case ok := <-monitorCh:
			if ok {
				f.setStage(StageSuccess, "")
				f.log.Info("[%s] 登录成功", f.Phone)
			}
			return false
		case <-tick.C:
			if msg := pageLoginError(page); msg != "" {
				f.setStage(StageWaitingCode, "登录未成功："+msg+"，请重新输入验证码")
				f.log.Warn("[%s] 登录未成功: %s", f.Phone, msg)
				return true
			}
			f.setMsg(fmt.Sprintf("正在等待登录完成（剩 %d 秒，期间请勿重复提交）", int(remain.Seconds())))
		}
	}
}

// pageLoginError 读取登录页错误提示（验证码错误/过期等）；非登录页或无错误返回 ""
func pageLoginError(page *rod.Page) string {
	res, err := page.Eval(`() => {
		// 已离开登录页（登录成功跳转中）则不判错误
		if (!document.querySelector('#code') && !document.querySelector('#loginBtn')) return '';
		const inp = document.querySelector('#code');
		if (inp) {
			const ph = inp.placeholder || '';
			if (ph && /错误|不正确|过期|失效|频繁|过多|稍后|失败/.test(ph)) return ph;
		}
		for (const e of document.querySelectorAll('[class*="error"], [class*="tips"], [class*="toast"]')) {
			const t = (e.innerText || '').trim();
			if (t && t.length <= 30 && /错误|不正确|过期|失效/.test(t)) return t;
		}
		return '';
	}`)
	if err != nil {
		return ""
	}
	return res.Value.Str()
}
