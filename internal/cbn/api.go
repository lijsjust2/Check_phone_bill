package cbn

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/loggerx"
)

// 登录/查询接口路径（baseUrl=/contact-web）
const (
	apiImageCheckCode = "/api/login/getImageCheckCode" // GET 图片验证码（binary）
	apiGetVerifyCode  = "/api/login/getVerifyCode"     // 发送短信验证码
	apiGwLogin        = "/api/login/gwLogin"           // 登录（短信码/服务密码）
	apiBalanceFee     = "/api/busi/qryBalanceFee"      // 话费余额
	apiPhonePackInfo  = "/api/busi/qryPhonePackInfo"   // 套餐信息
	apiUserRes        = "/api/busi/qryUserRes"         // 余量（流量/语音/短信资源列表）
	apiBillInfo       = "/api/busi/qryBillInfo"        // 账单
	apiNumberOwner    = "/api/busi/qryNumberOwnership" // 归属地省份列表（公开）
)

// ErrSessionExpired 登录已过期（status 701）
var ErrSessionExpired = fmt.Errorf("广电登录已过期，请重新登录")

// LoginState gwLogin 成功产物
type LoginState struct {
	SessionID   string // 服务端会话 id（查询接口必填）
	LoginPhone  string
	IsGd        bool
	PhoneInfo   gjson.Result // phoneInfo 原始节点（mgmtProv/areaCode 等）
	CookiesJSON string       // 会话 cookie 序列化（Account.Cookie）
}

// FetchImageCaptcha 获取图片验证码（base64 data URI，可直接 <img src>）
func (c *Client) FetchImageCaptcha(ctx context.Context) (string, error) {
	data, ctype, err := c.getBinary(ctx, BaseURL+apiImageCheckCode)
	if err != nil {
		return "", fmt.Errorf("获取图片验证码失败: %w", err)
	}
	if len(data) < 100 {
		return "", fmt.Errorf("图片验证码响应异常（%d 字节）", len(data))
	}
	if ctype == "" {
		ctype = "image/png"
	}
	return "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// SendSmsCode 发送短信验证码（type 5 = 登录验证码）
func (c *Client) SendSmsCode(ctx context.Context, phone string, log *loggerx.Logger) error {
	resp, err := c.callAPI(ctx, apiGetVerifyCode, map[string]interface{}{
		"channelId":  ChannelID,
		"recieveNum": phone,
		"type":       5,
	})
	if err != nil {
		return err
	}
	if log != nil {
		log.Info("广电短信验证码发送响应: %s", truncate(resp.Raw, 300))
	}
	if !resp.IsOK() {
		return fmt.Errorf("短信发送失败：%s", resp.Message)
	}
	return nil
}

// GwLogin 短信验证码登录（loginType "5"）。
// imageCheckCode 为图片验证码（登录链路必填，与图片接口同会话）。
func (c *Client) GwLogin(ctx context.Context, phone, smsCode, imageCheckCode string, log *loggerx.Logger) (*LoginState, error) {
	// 登录请求不携带 sessionId（尚未取得）
	saved := c.SessionID
	c.SessionID = ""
	resp, err := c.callAPI(ctx, apiGwLogin, map[string]interface{}{
		"channelId":      ChannelID,
		"loginPhone":     phone,
		"loginPassWord":  smsCode,
		"loginType":      "5",
		"channelType":    "1",
		"imageCheckCode": imageCheckCode,
	})
	c.SessionID = saved
	if err != nil {
		return nil, err
	}
	if log != nil {
		log.Info("[%s] 广电登录响应: %s", phone, truncate(resp.Raw, 500))
	}
	if resp.IsSessionExpired() {
		return nil, ErrSessionExpired
	}
	if !resp.IsOK() {
		return nil, fmt.Errorf("%s", resp.Message)
	}
	res := gjson.Parse(resp.Raw)
	sid := res.Get("sessionId").Str
	if sid == "" {
		// 兼容嵌套形态
		sid = res.Get("data.sessionId").Str
	}
	if sid == "" {
		return nil, fmt.Errorf("登录成功但未返回 sessionId")
	}
	c.SessionID = sid
	return &LoginState{
		SessionID:   sid,
		LoginPhone:  res.Get("loginPhone").Str,
		IsGd:        res.Get("isGd").Str == "1",
		PhoneInfo:   res.Get("phoneInfo"),
		CookiesJSON: c.SerializeCookies(),
	}, nil
}

// QueryBalanceFee 话费余额查询（accessNum=登录手机号）
func (c *Client) QueryBalanceFee(ctx context.Context, phone string, log *loggerx.Logger) (gjson.Result, string, error) {
	resp, err := c.callAPI(ctx, apiBalanceFee, map[string]interface{}{
		"channelId": ChannelID,
		"accessNum": phone,
	})
	if err != nil {
		return gjson.Result{}, "", err
	}
	if log != nil {
		log.Info("[%s] 广通话费查询响应: %s", phone, truncate(resp.Raw, 500))
	}
	if resp.IsSessionExpired() {
		return gjson.Result{}, resp.Raw, ErrSessionExpired
	}
	if !resp.IsOK() {
		return gjson.Result{}, resp.Raw, fmt.Errorf("话费查询失败：%s", resp.Message)
	}
	return gjson.Parse(resp.Raw), resp.Raw, nil
}

// QueryPhonePackInfo 套餐信息查询（返回套餐名/开通日期等）
func (c *Client) QueryPhonePackInfo(ctx context.Context, phone string, log *loggerx.Logger) (gjson.Result, string, error) {
	resp, err := c.callAPI(ctx, apiPhonePackInfo, map[string]interface{}{
		"channelId": ChannelID,
	})
	if err != nil {
		return gjson.Result{}, "", err
	}
	if log != nil {
		log.Info("[%s] 广电套餐查询响应: %s", phone, truncate(resp.Raw, 500))
	}
	if resp.IsSessionExpired() {
		return gjson.Result{}, resp.Raw, ErrSessionExpired
	}
	if !resp.IsOK() {
		return gjson.Result{}, resp.Raw, fmt.Errorf("套餐查询失败：%s", resp.Message)
	}
	return gjson.Parse(resp.Raw), resp.Raw, nil
}

// QueryUserRes 余量查询（流量/语音/短信资源列表）。
// 响应 userResList：itemTypeCode 3=流量(KB) 2=语音(分钟)；highFee=总量 balance=剩余 addupValue=已用。
func (c *Client) QueryUserRes(ctx context.Context, phone string, log *loggerx.Logger) (gjson.Result, string, error) {
	resp, err := c.callAPI(ctx, apiUserRes, map[string]interface{}{
		"channelId": ChannelID,
		"sessionId": c.SessionID,
		"accessNum": phone,
	})
	if err != nil {
		return gjson.Result{}, "", err
	}
	if log != nil {
		log.Info("[%s] 广电余量查询响应: %s", phone, truncate(resp.Raw, 500))
	}
	if resp.IsSessionExpired() {
		return gjson.Result{}, resp.Raw, ErrSessionExpired
	}
	if !resp.IsOK() {
		return gjson.Result{}, resp.Raw, fmt.Errorf("余量查询失败：%s", resp.Message)
	}
	return gjson.Parse(resp.Raw), resp.Raw, nil
}

// QueryBillInfo 账单查询（尽力读取，失败不阻断主流程）
func (c *Client) QueryBillInfo(ctx context.Context, phone string, log *loggerx.Logger) (gjson.Result, string, error) {
	resp, err := c.callAPI(ctx, apiBillInfo, map[string]interface{}{
		"channelId": ChannelID,
		"accessNum": phone,
	})
	if err != nil {
		return gjson.Result{}, "", err
	}
	if log != nil {
		log.Info("[%s] 广电账单查询响应: %s", phone, truncate(resp.Raw, 500))
	}
	if resp.IsSessionExpired() {
		return gjson.Result{}, resp.Raw, ErrSessionExpired
	}
	if !resp.IsOK() {
		return gjson.Result{}, resp.Raw, fmt.Errorf("账单查询失败：%s", resp.Message)
	}
	return gjson.Parse(resp.Raw), resp.Raw, nil
}

// QueryNumberOwnership 归属地省份列表（登录前公开接口，连通性自检用）
func (c *Client) QueryNumberOwnership(ctx context.Context) (gjson.Result, error) {
	resp, err := c.callAPI(ctx, apiNumberOwner, map[string]interface{}{
		"channelId": ChannelID,
	})
	if err != nil {
		return gjson.Result{}, err
	}
	if !resp.IsOK() {
		return gjson.Result{}, fmt.Errorf("%s", resp.Message)
	}
	return gjson.Parse(resp.Raw), nil
}
