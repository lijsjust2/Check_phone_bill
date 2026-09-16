package unicom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/loggerx"
)

// 联通通道（2026-09 起默认：短信验证码登录，纯 HTTP，无浏览器/滑块）：
// 登录：POST sendRadomNum.htm（手机号 RSA 加密）→ 下发短信验证码；再 POST
//   radomLogin.htm（手机号 + 验证码均 RSA 加密，loginStyle=0）完成登录，
//   响应返回 token_online 并 Set-Cookie 写入 .10010.com 会话 Cookie（见 smslogin.go）。
// 查询：凭登录后捕获的会话 Cookie 直连 m.client.10010.com 的 servicequerybusiness
//   接口（话费/余量），无第三方网关、无需浏览器、无需滑块。
//
// 注意：mxx 域接口只认 JUT，而短信登录产出的是 m.client 会话 Cookie，因此查询必须走
// m.client 域（同一 servicequerybusiness 后端，仅鉴权 Cookie 不同）。

const (
	// 余量查询（响应 flowSumList 单位 MB：flowtype 1=通用 2=专属 3=其他）
	webFlowLeftURL = "https://m.client.10010.com/servicequerybusiness/operationservice/queryOcsPackageFlowLeftContentRevisedInJune"
	// 话费余额查询（顶层字段：curntbalancecust 当前可用余额 / totalrealfee 实时话费 /
	// allbillfee 本月账单 / monthlyRechargeBill 本月存入）
	webBalanceURL = "https://m.client.10010.com/servicequerybusiness/balancenew/accountBalancenew.htm"

	webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// unicomReferer m.client 接口校验的 Referer/Origin（APP 掌厅域）
const unicomReferer = "https://m.client.10010.com/"

// ErrCookieInvalid 会话已失效（服务端返回纯文本 999999 或 code!=0000），需重新登录
var ErrCookieInvalid = errors.New("登录已失效，请重新登录")

var httpClient = &http.Client{Timeout: 30 * time.Second}

// postWebWT m.client 域 servicequerybusiness 表单查询（短信登录通道：version=WT，
// 票据字段留空，仅会话 Cookie 鉴权）。cookie 为登录后捕获的 .10010.com 会话 Cookie 串。
func postWebWT(ctx context.Context, rawURL, cookie string) (string, error) {
	form := url.Values{
		"duanlianjieabc":  {""},
		"channelCode":     {""},
		"serviceType":     {""},
		"saleChannel":     {""},
		"externalSources": {""},
		"contactCode":     {""},
		"ticket":          {""},
		"ticketPhone":     {""},
		"ticketChannel":   {""},
		"version":         {"WT"},
		"userNumber":      {""},
		"language":        {"chinese"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Origin", unicomReferer)
	req.Header.Set("Referer", unicomReferer)
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

// checkWebCode 网页通道响应判定（code="0000" 正常；纯文本 999999 = JUT 失效）
func checkWebCode(body, scene string) (gjson.Result, error) {
	if strings.TrimSpace(body) == "999999" {
		return gjson.Result{}, ErrCookieInvalid
	}
	res := gjson.Parse(body)
	switch res.Get("code").Str {
	case "0000":
		return res, nil
	case "999999", "999998":
		return res, ErrCookieInvalid
	}
	if strings.Contains(body, "沃妹陪着您一起等待") {
		return res, fmt.Errorf("联通服务暂不可用，请稍后再试")
	}
	desc := res.Get("desc").Str
	if desc == "" {
		desc = res.Get("msg").Str // 话费接口（accountBalancenew）错误文案在 msg 字段
	}
	if desc == "" {
		desc = "未知错误 " + res.Get("code").Str
	}
	return res, fmt.Errorf("%s失败: %s", scene, desc)
}

// QueryWebFlowLeft 余量查询（queryOcsPackageFlowLeftContentRevisedInJune，MB 单位）
func QueryWebFlowLeft(ctx context.Context, phone, cookie string, log *loggerx.Logger) (gjson.Result, string, error) {
	body, err := postWebWT(ctx, webFlowLeftURL, cookie)
	if err != nil {
		return gjson.Result{}, "", fmt.Errorf("余量查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[%s] 联通余量查询响应: %s", phone, truncate(body, 300))
	}
	res, err := checkWebCode(body, "余量查询")
	return res, body, err
}

// QueryWebBalance 话费余额查询（accountBalancenew.htm，与余量查询同域同 Cookie）
func QueryWebBalance(ctx context.Context, phone, cookie string, log *loggerx.Logger) (gjson.Result, error) {
	body, err := postWebWT(ctx, webBalanceURL, cookie)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("话费查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[%s] 联通话费查询响应: %s", phone, truncate(body, 300))
	}
	res, err := checkWebCode(body, "话费查询")
	return res, err
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
