package unicom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/loggerx"
)

// 联通微信小程序通道（2026-09 起唯一通道：OpenID 凭证，纯 HTTP，无浏览器/短信/风控）。
//
// 登录（换取查询票据与掌厅会话）：
//  1. POST https://mina.10010.com/wxapplet/weixinNew/getTicket
//     请求体 {"openId":"<OPENID>","channel":"wxmini"} → 返回 {"code":"0000","data":"<ticket>"}
//  2. GET  https://mxx.client.10010.com/servicebusiness/wx/serviceEntrance
//     ?ticket=<ticket>&servicecode=YH10007&ticketChannel=XCXSYHF
//     → Set-Cookie 写入 microHallUser / microHallAccessToken（掌厅会话 cookie）
//
// 查询（凭 ticket + ticketPhone + 上述 cookie，均直连 mxx.client 掌厅接口）：
//   - 余量：POST .../servicequerybusiness/operationservice/queryOcsPackageFlowLeftContentRevisedInJune
//   - 话费：POST .../servicequerybusiness/balancenew/accountBalancenew.htm
//
// 与旧通道的关键差异：**凭证是长期稳定的 OpenID**，ticket 每次查询现取现用，
// 因此不存在"登录态过期"，无需保活，也不依赖手机号/短信验证码，更不触发安全风控。
//
// 参考实现：GitHub Cyborg2017/ha_unicom_bill（Home Assistant 联通集成）。

const (
	// getTicket OpenID → 查询票据
	wxGetTicketURL = "https://mina.10010.com/wxapplet/weixinNew/getTicket"
	// serviceEntrance ticket → 掌厅会话 cookie（microHallUser / microHallAccessToken）
	wxServiceEntranceURL = "https://mxx.client.10010.com/servicebusiness/wx/serviceEntrance"
	// queryGoodsList 读取账号下的完整手机号（用于登录时校验，尽力调用）
	wxGoodsListURL = "https://mina.10010.com/wxapplet/weixinNew/queryGoodsList"

	// 余量查询（响应 flowSumList 单位 MB：flowtype 1=通用 2=专属 3=其他）
	wxFlowLeftURL = "https://mxx.client.10010.com/servicequerybusiness/operationservice/queryOcsPackageFlowLeftContentRevisedInJune"
	// 话费余额查询（顶层字段：curntbalancecust 当前可用余额 / totalrealfee 实时话费 /
	// allbillfee 本月账单 / monthlyRechargeBill 本月存入）
	wxBalanceURL = "https://mxx.client.10010.com/servicequerybusiness/balancenew/accountBalancenew.htm"

	// serviceEntrance 的业务编码与渠道标识
	wxServiceCode      = "YH10007"
	wxEntranceChannel  = "XCXSYHF"
	wxFlowTicketChan   = "XCXYLCXYY" // 余量接口的 ticketChannel
	wxBalanceTickChan  = "XCXSYHF"   // 话费接口的 ticketChannel
	wxMiniProgramChan  = "wxmini"
	wxRequestChannelVl = "client" // 话费接口额外要求 channel=client

	// 微信小程序环境 UA（网关按此识别小程序来源）
	wxUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36 " +
		"MicroMessenger/7.0.20.1781(0x6700143B) NetType/WIFI " +
		"MiniProgramEnv/Windows WindowsWechat/WMPF"

	// 微信小程序 Referer（联通小程序 appid wxa03c1c5e73b8e9a8）
	wxReferer = "https://servicewechat.com/wxa03c1c5e73b8e9a8/163/page-frame.html"
)

var (
	// ErrTicketInvalid 票据/会话已失效（服务端返回 999999 等），需重新换取
	ErrTicketInvalid = errors.New("登录票据已失效，请重新登录")
	// ErrOpenIDInvalid OpenID 无效或已失效（getTicket 返回非 0000）
	ErrOpenIDInvalid = errors.New("OpenID 无效或已失效，请重新抓包获取")
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ---------- 登录票据 ----------

// getTicket 用 OpenID 换取查询票据（ticket）。
// 注意：此接口的请求体字段名是 openId（大写 I），与其他接口的 openid 不同。
func getTicket(ctx context.Context, openid string) (string, error) {
	body, err := postJSON(ctx, wxGetTicketURL, map[string]string{
		"openId":  openid,
		"channel": wxMiniProgramChan,
	})
	if err != nil {
		return "", fmt.Errorf("获取票据请求失败: %w", err)
	}
	r := gjson.Parse(body)
	if code := r.Get("code").Str; code != "0000" {
		desc := unicomErrText(r)
		if desc == "" {
			desc = "code=" + code
		}
		return "", fmt.Errorf("%w（%s）", ErrOpenIDInvalid, desc)
	}
	ticket := r.Get("data").Str
	if ticket == "" {
		return "", fmt.Errorf("%w（服务端未返回 ticket）", ErrOpenIDInvalid)
	}
	return ticket, nil
}

// serviceEntrance 用 ticket 换取掌厅会话 cookie（microHallUser / microHallAccessToken）。
// 尽力而为：失败返回空串，查询仍会尝试（部分环境仅凭 ticket 即可）。
func serviceEntrance(ctx context.Context, ticket string) string {
	u := wxServiceEntranceURL +
		"?ticket=" + url.QueryEscape(ticket) +
		"&servicecode=" + wxServiceCode +
		"&ticketChannel=" + wxEntranceChannel
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	setWxHeaders(req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	var user, access string
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "microHallUser":
			user = c.Value
		case "microHallAccessToken":
			access = c.Value
		}
	}
	var parts []string
	if user != "" {
		parts = append(parts, "microHallUser="+user)
	}
	if access != "" {
		parts = append(parts, "microHallAccessToken="+access)
	}
	return strings.Join(parts, "; ")
}

// queryGoodsPhone 读取该 OpenID 名下绑定的完整手机号（登录时校验用；失败返回空串）。
func queryGoodsPhone(ctx context.Context, openid string) string {
	body, err := postJSON(ctx, wxGoodsListURL, map[string]string{
		"openid":  openid,
		"channel": wxMiniProgramChan,
	})
	if err != nil {
		return ""
	}
	r := gjson.Parse(body)
	if r.Get("code").Str != "0000" {
		return ""
	}
	for _, item := range r.Get("data.res").Array() {
		if n := item.Get("mainNumber").Str; len(n) == 11 && strings.HasPrefix(n, "1") {
			return n
		}
	}
	return ""
}

// ---------- 查询 ----------

// ticketPhone 掌厅接口要求的"票据手机号"占位值：wx + 毫秒时间戳（小程序端行为）。
func ticketPhone() string {
	return "wx" + strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// QueryFlowLeft 余量查询（queryOcsPackageFlowLeftContentRevisedInJune，MB 单位）
func QueryFlowLeft(ctx context.Context, ticket, tp, cookie string, log *loggerx.Logger) (gjson.Result, string, error) {
	body, err := postUnicomForm(ctx, wxFlowLeftURL, flowLeftForm(ticket, tp), cookie)
	if err != nil {
		return gjson.Result{}, "", fmt.Errorf("余量查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[ticket=%s...] 联通余量查询响应: %s", truncate(ticket, 12), truncate(body, 300))
	}
	res, err := checkWebCode(body, "余量查询")
	return res, body, err
}

// QueryBalance 话费余额查询（accountBalancenew.htm，与余量查询同域同凭证）
func QueryBalance(ctx context.Context, ticket, tp, cookie string, log *loggerx.Logger) (gjson.Result, error) {
	body, err := postUnicomForm(ctx, wxBalanceURL, balanceForm(ticket, tp), cookie)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("话费查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[ticket=%s...] 联通话费查询响应: %s", truncate(ticket, 12), truncate(body, 300))
	}
	return checkWebCode(body, "话费查询")
}

// flowLeftForm 余量查询表单（小程序端字段）
func flowLeftForm(ticket, tp string) url.Values {
	return url.Values{
		"duanlianjieabc":  {""},
		"channelCode":     {""},
		"serviceType":     {""},
		"saleChannel":     {""},
		"externalSources": {""},
		"contactCode":     {""},
		"ticket":          {ticket},
		"ticketPhone":     {tp},
		"ticketChannel":   {wxFlowTicketChan},
		"language":        {"chinese"},
	}
}

// balanceForm 话费查询表单（小程序端字段；额外要求 channel=client）
func balanceForm(ticket, tp string) url.Values {
	v := flowLeftForm(ticket, tp)
	v.Set("ticketChannel", wxBalanceTickChan)
	v.Set("channel", wxRequestChannelVl)
	return v
}

// ---------- HTTP 基础设施 ----------

// postJSON 提交 JSON 请求体，返回响应文本。
// mina.10010.com 域挂阿里云 WAF：必须先 GET 同路径领 acw_tc 会话 cookie 再 POST，
// 否则会被拦截成「温馨提示」页（curl/schannel TLS 指纹会被拦，Go 标准库指纹可过，已实测）。
func postJSON(ctx context.Context, rawURL string, payload interface{}) (string, error) {
	// WAF 预热：GET 同路径领 acw_tc
	req0, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	setWxHeaders(req0)
	resp0, err := httpClient.Do(req0)
	if err != nil {
		return "", fmt.Errorf("mina 域 WAF 预热失败: %w", err)
	}
	var acw []string
	for _, c := range resp0.Cookies() {
		acw = append(acw, c.Name+"="+c.Value)
	}
	_, _ = io.Copy(io.Discard, resp0.Body)
	_ = resp0.Body.Close()

	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	setWxHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	if len(acw) > 0 {
		req.Header.Set("Cookie", strings.Join(acw, "; "))
	}
	return doRead(req)
}

// setWxHeaders 微信小程序通道公共请求头
func setWxHeaders(req *http.Request) {
	req.Header.Set("User-Agent", wxUA)
	req.Header.Set("Referer", wxReferer)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh-Hans;q=0.9")
}

// postUnicomForm 提交掌厅表单查询（application/x-www-form-urlencoded，携带掌厅会话 cookie）
func postUnicomForm(ctx context.Context, rawURL string, form url.Values, cookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", wxUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh-Hans;q=0.9")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return doRead(req)
}

func doRead(req *http.Request) (string, error) {
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

// ---------- 响应判定 ----------

// checkWebCode 掌厅查询响应判定：
//   - code=0000 → 正常
//   - code=999999/999998 或纯文本 999999 → 票据失效
//   - 无 code 字段但含业务数据 → 视为正常（部分接口不回 code）
//   - 其余按错误处理，取 desc/dsc/mainDesc/msg 文案
func checkWebCode(body, scene string) (gjson.Result, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "999999" {
		return gjson.Result{}, ErrTicketInvalid
	}
	res := gjson.Parse(body)
	code := res.Get("code").Str
	switch code {
	case "0000":
		return res, nil
	case "999999", "999998":
		return res, ErrTicketInvalid
	}
	if code == "" && looksLikePayload(res) {
		return res, nil
	}
	if strings.Contains(body, "沃妹陪着您一起等待") {
		return res, fmt.Errorf("联通服务暂不可用，请稍后再试")
	}
	desc := unicomErrText(res)
	if desc == "" && code == "" {
		desc = truncate(trimmed, 120)
	}
	if desc == "" {
		desc = "未知错误 " + code
	}
	return res, fmt.Errorf("%s失败: %s", scene, desc)
}

// looksLikePayload 判断响应是否已含业务数据（用于兼容不回 code 的接口）
func looksLikePayload(res gjson.Result) bool {
	for _, k := range []string{"flowSumList", "resources", "unshared", "shareData", "allUserFlow", "curntbalancecust", "data"} {
		if res.Get(k).Exists() {
			return true
		}
	}
	return false
}

// unicomErrText 提取联通错误描述（不同接口字段名不一致：desc / dsc / mainDesc / msg / message）
func unicomErrText(r gjson.Result) string {
	for _, k := range []string{"desc", "dsc", "mainDesc", "msg", "message"} {
		if v := r.Get(k).Str; v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
