package unicom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/loggerx"
)

const (
	baseURL     = "https://m.client.10010.com"
	onlineURL   = baseURL + "/mobileService/onLine.htm"
	flowLeftURL = baseURL + "/servicequerybusiness/operationservice/queryOcsPackageFlowLeftContentRevisedInJune"
	myInfoURL   = baseURL + "/servicequerybusiness/query/myInformation"

	// 开源项目 unicomvue（github.com/AliYa-chen/unicomvue，活跃维护中）提供的
	// 公共登录网关：代理联通 ECS 体系「发码 → 腾讯滑块校验 → 短信登录」协议。
	// 联通官方接口已被滑块风控全面覆盖（纯 HTTP 直连被 WAF 拦截），登录走网关，
	// 拿到 ecs_token 后查询仍直连联通官方接口。
	gwBaseURL = "https://networkapi.2t.hk"
	gwSendURL = gwBaseURL + "/gettoken/?action=send"
	gwValidURL = gwBaseURL + "/gettoken/?action=validate"
	gwLoginURL = gwBaseURL + "/gettoken/?action=login"

	// 腾讯滑块 appid（联通 ECS 体系，前端弹出 TJCaptcha 用，与网关配套）
	CaptchaAppID = "195809716"
)

// ErrCookieInvalid Cookie 已失效（999999/999998），需要 onLine.htm 刷新
var ErrCookieInvalid = errors.New("登录 Cookie 已失效")

var httpClient = &http.Client{Timeout: 30 * time.Second}

// LoginState 联通登录成功产物（官网协议仅 Cookie；APP 协议存量账号含 token/appId）
type LoginState struct {
	AppID       string
	TokenOnline string
	Cookie      string
}

// postForm 表单 POST（application/x-www-form-urlencoded），返回响应体文本与 set-cookie
func postForm(ctx context.Context, rawURL string, form url.Values, ua string, cookie string) (body string, setCookie string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	// 多条 Set-Cookie 头合并（只取第一条会丢 cookie）
	return string(b), strings.Join(resp.Header.Values("Set-Cookie"), "; "), nil
}

// setCookieToCookie 清洗 Set-Cookie：去掉 Domain/Path 属性，拼成请求可用的 Cookie 串。
// 兼容逗号拼接的多条 cookie（"Path=/, next=val" 形式保留逗号后的部分）。
func setCookieToCookie(sc string) string {
	parts := strings.Split(sc, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		lower := strings.ToLower(p)
		if strings.HasPrefix(lower, "domain=") || strings.HasPrefix(lower, "path=") {
			// 属性值后可能跟逗号分隔的下一条 cookie：保留其后的部分
			if i := strings.Index(p, ","); i >= 0 {
				if rest := strings.TrimSpace(p[i+1:]); rest != "" {
					out = append(out, rest)
				}
			}
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "; ")
}

// KeepOnline 会话维持（onLine.htm）：token_online 换新 Cookie 与新 token，失败返回错误
// （仅存量 APP 协议账号使用；官网登录账号无 token_online，Cookie 失效需重新登录）
func KeepOnline(ctx context.Context, appID, tokenOnline string, log *loggerx.Logger) (*LoginState, error) {
	form := url.Values{
		"appId":        {appID},
		"token_online": {tokenOnline},
		"version":      {"iphone_c@9.0100"},
	}
	body, setCookie, err := postForm(ctx, onlineURL, form, "", "")
	if err != nil {
		return nil, fmt.Errorf("会话维持请求失败: %w", err)
	}
	if log != nil {
		log.Info("联通会话维持响应: %s", truncate(body, 300))
	}
	res := gjson.Parse(body)
	if res.Get("code").Str != "0" {
		msg := res.Get("dsc").Str
		if msg == "" {
			msg = "未知错误"
		}
		return nil, fmt.Errorf("会话维持失败: %s", msg)
	}
	cookie := setCookieToCookie(setCookie)
	if cookie == "" {
		return nil, fmt.Errorf("会话维持返回 Cookie 为空")
	}
	return &LoginState{
		AppID:       appID,
		TokenOnline: res.Get("token_online").Str,
		Cookie:      cookie,
	}, nil
}

// postWithCookie 带 Cookie 的 POST（查询类接口：无表单体）
func postWithCookie(ctx context.Context, rawURL, cookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// checkQueryCode 查询类接口公共错误码判定（code="0000" 正常）
func checkQueryCode(phone, body, scene string) (gjson.Result, error) {
	res := gjson.Parse(body)
	code := res.Get("code").Str
	switch code {
	case "0000":
		return res, nil
	case "999999", "999998":
		return res, ErrCookieInvalid
	case "4114030182", "9999", "9998", "0001":
		return res, fmt.Errorf("联通系统升级中（%s），请稍后再试", code)
	}
	if strings.Contains(body, "沃妹陪着您一起等待") {
		return res, fmt.Errorf("联通服务暂不可用，请稍后再试")
	}
	desc := res.Get("desc").Str
	if desc == "" {
		desc = "未知错误 " + code
	}
	return res, fmt.Errorf("%s失败: %s", scene, desc)
}

// QueryFlowLeft 流量余量查询（queryOcsPackageFlowLeftContentRevisedInJune）
func QueryFlowLeft(ctx context.Context, phone, cookie string, log *loggerx.Logger) (gjson.Result, string, error) {
	body, err := postWithCookie(ctx, flowLeftURL, cookie)
	if err != nil {
		return gjson.Result{}, "", fmt.Errorf("余量查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[%s] 联通余量查询响应: %s", phone, truncate(body, 300))
	}
	res, err := checkQueryCode(phone, body, "余量查询")
	return res, body, err
}

// QueryMyInfo 个人信息查询（myInformation，套餐名称尽力读取）
func QueryMyInfo(ctx context.Context, phone, cookie string, log *loggerx.Logger) string {
	body, err := postWithCookie(ctx, myInfoURL, cookie)
	if err != nil {
		return ""
	}
	res, err2 := checkQueryCode(phone, body, "个人信息查询")
	if err2 != nil {
		if log != nil && err2 != ErrCookieInvalid {
			log.Info("[%s] 联通套餐名称获取失败: %v", phone, err2)
		}
		return ""
	}
	return res.Get("data.myPackage.productname").Str
}

// probeWebSendMsg 官网 SendMSG 接口连通性探测（不带滑块票据，服务端应返回 resultCode=7001）
func probeWebSendMsg(ctx context.Context, phone string) (string, error) {
	u := "https://uac.10010.com/portal/Service/SendMSG?callback=probe&req_time=" +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + "&mobile=" + phone
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://uac.10010.com/portal/hallLogin")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------- 网关登录协议（unicomvue 公共网关） ----------

// gwResp 网关统一响应：status ∈ success | need_captcha | fail
type gwResp struct {
	Status      string `json:"status"`
	Msg         string `json:"msg"`
	Mobile      string `json:"mobile"`      // need_captcha 时返回（携号转网归属提示等）
	ResultToken string `json:"resultToken"` // validate 成功后的票据，重发验证码时携带
	EcsToken    string `json:"ecs_token"`   // login 成功后的查询令牌（Cookie）
	OnlinToken  string `json:"onlin_token"` // login 成功后的会话维持令牌（token_online）
}

// gwPost 网关 JSON POST（Content-Type: text/plain 规避预检，与 unicomvue 前端一致）
func gwPost(ctx context.Context, rawURL string, payload map[string]string) (*gwResp, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", gwBaseURL)
	req.Header.Set("Referer", gwBaseURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("网关请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("网关返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 120))
	}
	var g gwResp
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("网关响应解析失败: %s", truncate(string(body), 120))
	}
	return &g, nil
}

// gwSend 发送短信验证码（resultToken 非空表示滑块校验后重发）
// 注意：网关期望的 appId 字段名为全小写 "appid"（对齐 unicomvue 前端 normalizeLoginPayload）
func gwSend(ctx context.Context, phone, appID, deviceID, resultToken string) (*gwResp, error) {
	return gwPost(ctx, gwSendURL, map[string]string{
		"phone":       phone,
		"appid":       appID,
		"deviceId":    deviceID,
		"resultToken": resultToken,
	})
}

// gwValidate 提交腾讯滑块票据校验（mobile 为 send 返回的归属提示字段）
func gwValidate(ctx context.Context, ticket, randstr, mobile, phone, appID, deviceID string) (*gwResp, error) {
	return gwPost(ctx, gwValidURL, map[string]string{
		"ticket":   ticket,
		"randstr":  randstr,
		"mobile":   mobile,
		"phone":    phone,
		"appid":    appID,
		"deviceId": deviceID,
	})
}

// gwLogin 短信验证码登录，成功返回 ecs_token / onlin_token
func gwLogin(ctx context.Context, phone, code, appID, deviceID string) (*gwResp, error) {
	return gwPost(ctx, gwLoginURL, map[string]string{
		"phone":    phone,
		"code":     code,
		"appid":    appID,
		"deviceId": deviceID,
	})
}

// generateAppID 生成联通 APP 身份 appId（对齐 unicomvue 前端的随机数填充模式）
func generateAppID() string {
	d := func() string { return strconv.Itoa(rand.Intn(10)) }
	return d() + "f" + d() + "af" + d() + d() + "ad" + d() +
		"912d306b5053abf90c7ebbb695887bc870ae0706d573c348539c26c5c0a878641fcc0d3e90acb9be1e6ef858a59af546f3c826988332376b7d18c8ea2398ee3a9c3db947e2471d32a49612"
}

// generateDeviceID 生成 32 位十六进制设备 ID
func generateDeviceID() string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, 32)
	for i := range b {
		b[i] = hexDigits[rand.Intn(16)]
	}
	return string(b)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
