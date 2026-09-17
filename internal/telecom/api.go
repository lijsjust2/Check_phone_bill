package telecom

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/loggerx"
)

const (
	loginURL   = "https://appgologin.189.cn:9031/login/client/userLoginNormal"
	queryURL   = "https://appfuwu.189.cn:9021/query/qryImportantData"
	clientType = "#12.2.0#channel50#iPhone 14 Pro#"

	// 查询接口省份/城市缺省值（Python 版一致）
	defaultProvinceCode = "600101"
	defaultCityCode     = "8441900"
)

// ErrTokenExpired token 已失效（X201），需要重新登录
var ErrTokenExpired = errors.New("登录已过期（token 失效）")

// ErrDeviceUntrusted 3006 设备未信任：需先通过短信授权注册设备获取 androidId
var ErrDeviceUntrusted = errors.New("设备未信任（3006），需要短信验证绑定设备")

// LoginState 登录成功产物
type LoginState struct {
	Token        string
	ProvinceCode string
	CityCode     string
	ProvinceName string // 响应可能携带，尽力读取用于展示
}

func commonHeaders() map[string]string {
	return map[string]string{
		"Accept":          "application/json",
		"Content-Type":    "application/json; charset=UTF-8",
		"Connection":      "Keep-Alive",
		"Accept-Encoding": "gzip",
	}
}

// postJSON 通用请求：返回响应体文本。
// 显式携带 Accept-Encoding: gzip 时 Go transport 不会透明解压，需手动解压
// （查询网关 appfuwu.189.cn 返回 gzip，登录网关 appgologin.189.cn 不压缩）。
func postJSON(ctx context.Context, url string, body map[string]interface{}) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	for k, v := range commonHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b2, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Content-Encoding 标注 gzip，或响应体以 gzip 魔数（0x1f 0x8b）开头时解压
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") ||
		(len(b2) > 2 && b2[0] == 0x1f && b2[1] == 0x8b) {
		if zr, zerr := gzip.NewReader(bytes.NewReader(b2)); zerr == nil {
			if dec, derr := io.ReadAll(zr); derr == nil {
				b2 = dec
			}
			_ = zr.Close()
		}
	}
	return string(b2), nil
}

// headerInfos 公共请求头字段（token 为空时省略该字段，与官方客户端一致）
func headerInfos(code, ts, token, phone string) map[string]interface{} {
	h := map[string]interface{}{
		"code":           code,
		"clientType":     clientType,
		"timestamp":      ts,
		"shopId":         "20002",
		"source":         "110003",
		"sourcePassword": "Sid98s",
		"userLoginName":  TransNumber(phone, true),
	}
	if token != "" {
		h["token"] = token
	}
	return h
}

// DoLogin 服务密码登录（userLoginNormal），成功返回 token/省市代码。
// androidID 为短信授权注册的设备 id（可为空）；deviceUid 固定为 "3"+手机号，
// 避免每次随机生成被服务端判定为新设备（3006 设备未信任）。
func DoLogin(ctx context.Context, phone, password, androidID string, log *loggerx.Logger) (*LoginState, error) {
	ts := time.Now().Format("200601021504") + "00"
	const systemVersion = "15.4.0"
	deviceUID := "3" + phone
	// 签名用的设备标识：已注册设备取 androidId 前 12 位，否则退回手机号
	trusted := phone
	if androidID != "" {
		trusted = androidID[:12]
	}
	encStr := "iPhone 14 " + systemVersion + trusted + phone + ts + password + "0$$$0."
	cipher, err := EncryptRSA(encStr)
	if err != nil {
		return nil, fmt.Errorf("加密登录凭据失败: %w", err)
	}

	fieldData := map[string]interface{}{
		"accountType":                "",
		"authentication":             TransNumber(password, true),
		"deviceUid":                  deviceUID,
		"isChinatelecom":             "0",
		"loginAuthCipherAsymmertric": cipher,
		"loginType":                  "4",
		"phoneNum":                   TransNumber(phone, true),
		"systemVersion":              systemVersion,
	}
	if androidID != "" {
		fieldData["androidId"] = TransNumber(androidID, true)
	}

	body := map[string]interface{}{
		"content": map[string]interface{}{
			"fieldData": fieldData,
			"attach":    "iPhone",
		},
		"headerInfos": headerInfos("userLoginNormal", ts, "", phone),
	}

	text, err := postJSON(ctx, loginURL, body)
	if err != nil {
		if log != nil {
			log.Error("[%s] 电信登录请求失败: %v", phone, err)
		}
		return nil, fmt.Errorf("登录请求失败: %w", err)
	}
	if log != nil {
		log.Info("[%s] 电信登录响应: %s", phone, truncate(text, 500))
	}

	res := gjson.Parse(text)
	if res.Get("responseData.resultCode").Str != "0000" {
		// 3006：设备未信任，需要短信授权注册设备后携带 androidId 重试
		if res.Get("responseData.resultCode").Str == "3006" {
			return nil, ErrDeviceUntrusted
		}
		// 失败信息：loginFailResult 里通常有失败原因与累计失败次数
		fail := res.Get("responseData.data.loginFailResult")
		msg := fail.Get("loginFailReason").Str
		if msg == "" {
			msg = res.Get("responseData.resultDesc").Str
		}
		if msg == "" {
			msg = "手机号或服务密码错误"
		}
		if n := fail.Get("loginFailTime").Int(); n > 0 {
			msg += fmt.Sprintf("（已连续失败 %d 次，连续多次失败可能触发风控锁定）", n)
		}
		return nil, errors.New(msg)
	}

	succ := res.Get("responseData.data.loginSuccessResult")
	state := &LoginState{
		Token:        succ.Get("token").Str,
		ProvinceCode: succ.Get("provinceCode").Str,
		CityCode:     succ.Get("cityCode").Str,
		ProvinceName: succ.Get("provinceName").Str,
	}
	if state.Token == "" {
		return nil, errors.New("登录成功但未返回 token")
	}
	return state, nil
}

// qryResp 查询响应
type qryResp struct {
	Raw     string // 响应原文
	Data    gjson.Result
	Expired bool   // token 失效（X201）
	FailMsg string // 其他失败原因
}

// QryImportantData 查询话费/通话/流量（token 必填）
func QryImportantData(ctx context.Context, phone, token, provinceCode, cityCode string, log *loggerx.Logger) (*qryResp, error) {
	ts := time.Now().Format("200601021504") + "00"
	if provinceCode == "" {
		provinceCode = defaultProvinceCode
	}
	if cityCode == "" {
		cityCode = defaultCityCode
	}

	body := map[string]interface{}{
		"content": map[string]interface{}{
			"fieldData": map[string]interface{}{
				"provinceCode":   provinceCode,
				"cityCode":       cityCode,
				"shopId":         "20002",
				"isChinatelecom": "0",
				"account":        TransNumber(phone, true),
			},
		},
		"headerInfos": headerInfos("qryImportantData", ts, token, phone),
	}

	text, err := postJSON(ctx, queryURL, body)
	if err != nil {
		if log != nil {
			log.Error("[%s] 电信查询请求失败: %v", phone, err)
		}
		return nil, fmt.Errorf("查询请求失败: %w", err)
	}
	if log != nil {
		log.Info("[%s] 电信查询响应: %s", phone, truncate(text, 500))
	}

	res := gjson.Parse(text)
	out := &qryResp{Raw: text}
	data := res.Get("responseData.data")
	if data.Exists() && len(data.Raw) > 2 { // responseData.data 非空对象
		out.Data = data
		return out, nil
	}
	// 无 responseData 且 X201 → token 失效
	if res.Get("headerInfos.code").Str == "X201" {
		out.Expired = true
		return out, nil
	}
	out.FailMsg = res.Get("headerInfos.reason").Str
	if out.FailMsg == "" {
		out.FailMsg = "查询失败: " + truncate(text, 200)
	}
	return out, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
