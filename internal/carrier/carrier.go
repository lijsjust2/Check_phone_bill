// Package carrier 运营商统一抽象：登录会话、查询与注册表。
// mobile/unicom/telecom/cbn 各实现包通过 init() 注册 Provider，业务层只依赖本包。
package carrier

import (
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// 运营商代码（Account.Carrier 取值；空串视为 mobile 兼容旧数据）
const (
	Mobile  = "mobile"
	Unicom  = "unicom"
	Telecom = "telecom"
	Cbn     = "cbn"
)

// 登录流程阶段（三家共用，Web 端点轮询展示）
const (
	StageStarting         = "starting"           // 启动浏览器 / 发起登录
	StagePageLoading      = "page_loading"       // 打开登录页
	StageCodeSending      = "code_sending"       // 填手机号 / 发送验证码
	StageNeedImageCaptcha = "need_image_captcha" // 需要前端输入图片验证码（电信设备注册/广电登录）
	StageWaitingUser      = "waiting_user"       // 等待用户在弹出的浏览器窗口中完成登录（联通网页登录）
	StageWaitingCode      = "waiting_code"       // 等待用户输入短信验证码
	StageSendFailed       = "send_failed"        // 验证码发送失败（仍可手动输入）
	StageSubmitting       = "submitting"         // 已提交（验证码已填入 / 密码已提交），等待完成
	StageSuccess          = "success"
	StageError            = "error"
	StageClosed           = "closed" // 浏览器被关闭 / 用户取消
)

var stageText = map[string]string{
	StageStarting:         "正在启动登录...",
	StagePageLoading:      "正在打开登录页...",
	StageCodeSending:      "正在发送验证码...",
	StageNeedImageCaptcha: "需要图片验证码，请输入图片中的字符",
	StageWaitingUser:      "请在弹出的浏览器窗口中完成登录",
	StageWaitingCode:      "验证码已发送，请输入收到的短信验证码",
	StageSendFailed:       "验证码发送失败",
	StageSubmitting:       "验证码已提交，等待登录完成...",
	StageSuccess:          "登录成功",
	StageError:            "发生错误",
	StageClosed:           "登录已取消",
}

// StageText 阶段中文说明
func StageText(stage string) string {
	if t, ok := stageText[stage]; ok {
		return t
	}
	return stage
}

// carrierNames 运营商代码 → 中文名（静态映射，用于展示标注，不依赖 Provider 注册）
var carrierNames = map[string]string{
	Mobile:  "中国移动",
	Unicom:  "中国联通",
	Telecom: "中国电信",
	Cbn:     "中国广电",
}

// CarrierName 运营商代码 → 中文名；未知代码返回空串
func CarrierName(code string) string {
	return carrierNames[code]
}

// LoginSession 一次登录会话：
//   - 验证码型（移动/联通）：浏览器异步流程，验证码经 SubmitCode 注入
//   - 密码型（电信）：goroutine 内执行 HTTP 登录，SubmitCode 返回错误
type LoginSession interface {
	// SubmitCode 提交短信验证码（密码型会话返回错误）
	SubmitCode(code string) error
	// Cancel 取消登录（关闭浏览器 / 中止请求）
	Cancel()
	// Done 会话结束信号
	Done() <-chan struct{}
	// Status 读取当前阶段与提示（并发安全）
	Status() (stage, msg string)
	// CodeSubmitted 是否已提交过验证码（结束原因提示用）
	CodeSubmitted() bool
}

// ImageCaptchaSession 需要前端图片验证码交互的会话（电信设备注册）：
// 前端轮询到 StageNeedImageCaptcha 时展示 CaptchaImage 返回的图片，
// 用户输入的字符经 SubmitImageCaptcha 注入
type ImageCaptchaSession interface {
	CaptchaImage() string
	SubmitImageCaptcha(captcha string) error
}

// LoginStateSaver 密码型会话成功后回写登录态（电信 token/省份/城市）。
// Web 层在会话结束且阶段为 success 时 type-assert 调用。
type LoginStateSaver interface {
	SaveLogin(a *store.Account)
}

// LoginParams StartLogin 入参
type LoginParams struct {
	Phone     string
	Password  string // 电信服务密码；验证码型运营商忽略
	AndroidID string // 电信专用：已绑定设备 id（重新登录时复用，跳过设备注册）
	DataDir   string
	Headless  bool // 浏览器型：Web 面板无头 / CLI 有头
	Log       *loggerx.Logger
}

// Result 单账号查询产物（各运营商通用）
type Result struct {
	Phone  string
	Result *store.QueryResult
	Raw    map[string]string // 原始响应（调试/落盘）
	JSONFn string            // 落盘文件路径（可选）
	Err    string
	// UpdateAccount 查询过程中自愈了登录态（电信 token 失效自动重登续期）时的账号回写回调，
	// runner 在查询返回后应用；nil 表示无需回写
	UpdateAccount func(a *store.Account)
	// LoginExpired 本次失败属于「登录态已失效」（非网络抖动、非业务限流）。
	// runner 会据此发专用「登录态失效」推送，不受 alert_only 开关影响。
	LoginExpired bool
}

// MarkNotLoggedIn 查询过程中确认登录态已失效时调用：runner 会把账号置回「未登录」。
//
// 不这样做的话 HasLoginState 只在登录成功时置 true，会话静默过期后列表仍显示绿色
// 「已登录」，只有 last_error 里能看到查询失败——面板状态与真实可用性不一致。
// 与已有回写回调组合（如电信重登后写回新 token），不会互相覆盖。
func MarkNotLoggedIn(pr *Result) {
	prev := pr.UpdateAccount
	pr.UpdateAccount = func(a *store.Account) {
		if prev != nil {
			prev(a)
		}
		a.HasLoginState = false
	}
}

// Provider 运营商接入抽象
type Provider interface {
	// Code 运营商代码 "mobile" | "unicom" | "telecom"
	Code() string
	// Name 中文名
	Name() string
	// NeedsPassword 前端表单切换依据（电信 true：需服务密码）
	NeedsPassword() bool
	// NeedsSMSCode 前端验证码输入框显隐依据（移动/联通 true）
	NeedsSMSCode() bool
	// Query 查询余量：浏览器型读 dataDir 下登录态目录；HTTP 型读 acc.Token
	Query(acc *store.Account, dataDir string, log *loggerx.Logger) *Result
	// StartLogin 异步启动登录会话
	StartLogin(p LoginParams) (LoginSession, error)
}
