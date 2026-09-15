package cbn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// 广电网上营业厅基础信息
const (
	BaseSite = "https://www.10099.com.cn"
	BaseURL  = BaseSite + "/contact-web"
	// 前端全局渠道标识（common.js config.channelId）
	ChannelID = "cd_20220516_093342"
	// 前端请求附加版本号（ajaxPrefilter 注入）
	APIVersion = "1.1.7.0915_release"
	// 挑战页/接口通用 UA（Chrome 桌面）
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36"
)

// cookieJarEntry Cookie 持久化条目（Account.Cookie JSON 数组）
type cookieJarEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Client 广电会话客户端：cookie jar + WAF 自愈 + 加密请求管线
type Client struct {
	http *http.Client
	jar  *cookiejar.Jar
	// SessionID 服务端会话 id（gwLogin 返回；查询接口必填）
	SessionID string
}

// NewClient 创建空会话客户端（登录流程用）
func NewClient() (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		http: &http.Client{Timeout: 30 * time.Second, Jar: jar},
		jar:  jar,
	}, nil
}

// RestoreClient 从 Account.Cookie（JSON 数组）恢复会话
func RestoreClient(cookiesJSON string) (*Client, error) {
	c, err := NewClient()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cookiesJSON) == "" {
		return nil, fmt.Errorf("无广电登录态")
	}
	var entries []cookieJarEntry
	if err := json.Unmarshal([]byte(cookiesJSON), &entries); err != nil {
		return nil, fmt.Errorf("登录态解析失败: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("无广电登录态")
	}
	pu, _ := url.Parse(BaseSite)
	var cs []*http.Cookie
	for _, e := range entries {
		cs = append(cs, &http.Cookie{Name: e.Name, Value: e.Value, Path: "/"})
	}
	c.jar.SetCookies(pu, cs)
	return c, nil
}

// SerializeCookies 导出全部 cookie（JSON 数组，写回 Account.Cookie）
func (c *Client) SerializeCookies() string {
	pu, _ := url.Parse(BaseSite)
	entries := []cookieJarEntry{}
	for _, ck := range c.jar.Cookies(pu) {
		entries = append(entries, cookieJarEntry{Name: ck.Name, Value: ck.Value})
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(b)
}

// EnsureSession 访问登录页过 WAF 并建立 cookie（acw_tc/acw_sc__v2/瑞数 cookie/SERVERID）
func (c *Client) EnsureSession(ctx context.Context) error {
	_, err := c.get(ctx, BaseSite+"/login.html", map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9",
	})
	return err
}

// get GET 请求（WAF 挑战自动求解重试）
func (c *Client) get(ctx context.Context, u string, headers map[string]string) (string, error) {
	var body string
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", UserAgent)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return "", err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		body = string(b)
		if !IsWAFChallenge(body) || i == 2 {
			return body, nil
		}
		val, serr := SolveWAFChallenge(body, u, UserAgent)
		if serr != nil {
			return body, fmt.Errorf("WAF 求解失败: %w", serr)
		}
		pu, _ := url.Parse(u)
		c.jar.SetCookies(pu, []*http.Cookie{{Name: "acw_sc__v2", Value: val, Path: "/"}})
	}
	return body, nil
}

// APIResponse 通用响应（status 000000 成功 / 701 登录过期）
type APIResponse struct {
	Raw     string // 解密后的明文 JSON
	Status  string
	Message string
	Body    string // 原始响应体（可能是密文形态）
}

// IsOK 业务成功
func (r *APIResponse) IsOK() bool { return r.Status == "000000" }

// IsSessionExpired 登录已过期（701）
func (r *APIResponse) IsSessionExpired() bool { return r.Status == "701" }

// callAPI 加密请求管线：params + 版本/时间戳 → Access 签名 + RSA 加密
// body {"data":密文} → 响应单键密文时私钥解密。
func (c *Client) callAPI(ctx context.Context, path string, params map[string]interface{}) (*APIResponse, error) {
	params["v"] = APIVersion
	params["timestamp"] = time.Now().UnixMilli()
	if c.SessionID != "" {
		params["sessionId"] = c.SessionID
	}
	access, err := SignAccess(params)
	if err != nil {
		return nil, err
	}
	enc, err := EncodePayload(params)
	if err != nil {
		return nil, err
	}
	bodyStr := `{"data":"` + enc + `"}`

	var body string
	for i := 0; i < 3; i++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+path, strings.NewReader(bodyStr))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		req.Header.Set("Origin", BaseSite)
		req.Header.Set("Referer", BaseSite+"/login.html")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Access", access)
		resp, derr := c.http.Do(req)
		if derr != nil {
			return nil, derr
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(b)
		// POST 也可能触发 WAF 挑战（会话 cookie 过期时）
		if !IsWAFChallenge(body) || i == 2 {
			break
		}
		val, serr := SolveWAFChallenge(body, BaseURL+path, UserAgent)
		if serr != nil {
			break
		}
		pu, _ := url.Parse(BaseSite)
		c.jar.SetCookies(pu, []*http.Cookie{{Name: "acw_sc__v2", Value: val, Path: "/"}})
	}

	out := &APIResponse{Body: body}
	// 形态一：单键 {data:"<密文>"} → 私钥解密
	var parsed map[string]json.RawMessage
	if jerr := json.Unmarshal([]byte(body), &parsed); jerr != nil {
		return nil, fmt.Errorf("响应非 JSON: %s", truncate(body, 200))
	}
	if len(parsed) == 1 {
		if ds, ok := parsed["data"]; ok {
			var s string
			if json.Unmarshal(ds, &s) == nil && s != "" {
				pt, perr := DecryptLong(s)
				if perr != nil {
					return nil, fmt.Errorf("响应解密失败: %w", perr)
				}
				body = DecodePayload(pt)
				_ = json.Unmarshal([]byte(body), &parsed)
			}
		}
	}
	// 形态二：明文 JSON（含 status/message）
	if st, ok := parsed["status"]; ok {
		_ = json.Unmarshal(st, &out.Status)
	}
	if msg, ok := parsed["message"]; ok {
		_ = json.Unmarshal(msg, &out.Message)
	}
	out.Raw = body
	return out, nil
}

// getBinary GET 二进制（图片验证码），返回字节与 content-type
func (c *Client) getBinary(ctx context.Context, u string) ([]byte, string, error) {
	var data []byte
	var ctype string
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Referer", BaseSite+"/login.html")
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, "", err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ctype = resp.Header.Get("Content-Type")
		// 图片接口的 WAF 挑战返回 HTML：解出 cookie 后重试
		if resp.StatusCode == 200 && !strings.Contains(ctype, "text/html") {
			return b, ctype, nil
		}
		if i == 2 {
			return b, ctype, nil
		}
		html := string(b)
		if !IsWAFChallenge(html) {
			return b, ctype, nil
		}
		val, serr := SolveWAFChallenge(html, u, UserAgent)
		if serr != nil {
			return b, ctype, nil
		}
		pu, _ := url.Parse(u)
		c.jar.SetCookies(pu, []*http.Cookie{{Name: "acw_sc__v2", Value: val, Path: "/"}})
	}
	return data, ctype, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
