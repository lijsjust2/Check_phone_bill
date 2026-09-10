package telecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/loggerx"
)

// 设备注册网关：电信 3006 设备未信任时，通过网关完成
// 图片验证码 + 短信验证码登录，换取服务端信任的 androidId。
// 协议与公开站点（telecom.nufe.ccwu.cc / trycloudflare 镜像）一致。
const (
	enrollBaseURL = "https://telecom.nufe.ccwu.cc"
)

// enrollHTTP 网关专用客户端（普通 TLS，独立于电信旧网关的宽松配置）
var enrollHTTP = &http.Client{Timeout: 30 * time.Second}

// enrollPost 网关通用 POST：{"ok":true,...} 失败时返回 message
func enrollPost(ctx context.Context, path string, body map[string]string) (gjson.Result, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return gjson.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, enrollBaseURL+path, bytes.NewReader(b))
	if err != nil {
		return gjson.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := enrollHTTP.Do(req)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("网关请求失败（%s 需可达）: %w", enrollBaseURL, err)
	}
	defer resp.Body.Close()
	b2, err := io.ReadAll(resp.Body)
	if err != nil {
		return gjson.Result{}, err
	}
	res := gjson.ParseBytes(b2)
	if resp.StatusCode != http.StatusOK || !res.Get("ok").Bool() {
		msg := res.Get("message").Str
		if msg == "" {
			msg = fmt.Sprintf("网关响应异常（HTTP %d）: %s", resp.StatusCode, truncate(string(b2), 200))
		}
		return res, fmt.Errorf("%s", msg)
	}
	return res, nil
}

// EnrollCaptchaState 图片验证码会话
type EnrollCaptchaState struct {
	Flow  string // 网关流程标识（后续 send-code / login 携带）
	Image string // 图片验证码 data URI（前端直接展示）
}

// FetchEnrollCaptcha 获取图片验证码（绑定设备第一步）
func FetchEnrollCaptcha(ctx context.Context, phone string, log *loggerx.Logger) (*EnrollCaptchaState, error) {
	res, err := enrollPost(ctx, "/api/captcha", map[string]string{"phone": phone})
	if err != nil {
		return nil, err
	}
	state := &EnrollCaptchaState{
		Flow:  res.Get("flow").Str,
		Image: res.Get("image").Str,
	}
	if state.Flow == "" || state.Image == "" {
		return nil, fmt.Errorf("网关未返回验证码，请稍后重试")
	}
	if log != nil {
		log.Info("[%s] 电信设备注册：已获取图片验证码", phone)
	}
	return state, nil
}

// SendEnrollCode 校验图片验证码并发送短信（绑定设备第二步）
func SendEnrollCode(ctx context.Context, flow, captcha string, log *loggerx.Logger) (string, error) {
	res, err := enrollPost(ctx, "/api/send-code", map[string]string{"flow": flow, "captcha": captcha})
	if err != nil {
		return "", err
	}
	newFlow := res.Get("flow").Str
	if newFlow == "" {
		newFlow = flow
	}
	if log != nil {
		log.Info("电信设备注册：短信验证码已发送")
	}
	return newFlow, nil
}

// EnrollLogin 短信验证码登录换取 androidId（绑定设备第三步）
func EnrollLogin(ctx context.Context, flow, code string, log *loggerx.Logger) (string, error) {
	res, err := enrollPost(ctx, "/api/login", map[string]string{"flow": flow, "code": code})
	if err != nil {
		return "", err
	}
	androidID := res.Get("androidId").Str
	if androidID == "" {
		return "", fmt.Errorf("网关未返回 androidId，请稍后重试")
	}
	if log != nil {
		log.Info("电信设备注册：绑定成功（androidId 前 8 位 %s...）", androidID[:min(8, len(androidID))])
	}
	return androidID, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
