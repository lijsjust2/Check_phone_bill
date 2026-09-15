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

// 联通网页通道（2026-09 起唯一通道）：
// 登录：面板弹出真浏览器 → www.10010.com 自动跳 uac.10010.com 统一登录页
// （密码登录 / 随机密码登录任选，滑块与短信验证码在真实浏览器内完成）→ 登录成功后
// JUT Cookie 写入 .10010.com 域，面板拿到 JUT 并调用余量接口校验通过即登录完成
// （见 weblogin.go）。
// 查询：仅凭 JUT 一个 Cookie 直连 mxx 域 WT 表单接口（话费/余量），无第三方网关。
//
// 实测结论（2026-09-10）：mxx 域接口只认 JUT，SHAREJSESSIONID / acw_tc / piw /
// u_account / ecs_cook 等一概不需要；不带 Cookie 时返回纯文本 999999。

const (
	// 余量查询（响应 flowSumList 单位 MB：flowtype 1=通用 2=专属 3=其他）
	webFlowLeftURL = "https://mxx.client.10010.com/servicequerybusiness/operationservice/queryOcsPackageFlowLeftContentRevisedInJune"
	// 话费余额查询（顶层字段：curntbalancecust 当前可用余额 / totalrealfee 实时话费 /
	// allbillfee 本月账单 / monthlyRechargeBill 本月存入）
	webBalanceURL = "https://mxx.client.10010.com/servicequerybusiness/balancenew/accountBalancenew.htm"

	// 网厅入口（未登录自动跳统一登录页，登录成功后 JUT 落到 .10010.com 域）
	WebHallURL = "https://www.10010.com/"

	webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// webHallFallbacks 备用网厅/掌厅入口（主入口打不开时依次尝试）
var webHallFallbacks = []string{"https://iservice.10010.com/", "https://m.client.10010.com/"}

// ErrCookieInvalid JUT 已失效（服务端返回纯文本 999999），需重新登录
var ErrCookieInvalid = errors.New("登录已失效，请重新登录")

var httpClient = &http.Client{Timeout: 30 * time.Second}

// postWebWT mxx 域 WT 表单查询（网页通道：version=WT，票据字段留空，仅 JUT Cookie 鉴权）
func postWebWT(ctx context.Context, rawURL, jut string) (string, error) {
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
	req.Header.Set("Cookie", "JUT="+jut)
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Origin", "https://imgxx.client.10010.com")
	req.Header.Set("Referer", "https://imgxx.client.10010.com/")
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
func QueryWebFlowLeft(ctx context.Context, phone, jut string, log *loggerx.Logger) (gjson.Result, string, error) {
	body, err := postWebWT(ctx, webFlowLeftURL, jut)
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
func QueryWebBalance(ctx context.Context, phone, jut string, log *loggerx.Logger) (gjson.Result, error) {
	body, err := postWebWT(ctx, webBalanceURL, jut)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("话费查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[%s] 联通话费查询响应: %s", phone, truncate(body, 300))
	}
	res, err := checkWebCode(body, "话费查询")
	return res, err
}

// ValidJUT 校验 JUT 是否有效（登录会话启动时 / 诊断用；有效返回 nil）
func ValidJUT(ctx context.Context, jut string) error {
	body, err := postWebWT(ctx, webFlowLeftURL, jut)
	if err != nil {
		return err
	}
	_, cerr := checkWebCode(body, "登录态校验")
	return cerr
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
