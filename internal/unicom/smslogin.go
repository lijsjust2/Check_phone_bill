package unicom

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/store"
)

// 联通短信验证码登录（默认通道，告别网页滑块）：
//   - 发送：POST sendRadomNum.htm（手机号 RSA 加密）→ 下发短信验证码
//   - 登录：POST radomLogin.htm（手机号 + 验证码均 RSA 加密，loginStyle=0）
//     返回 token_online / ecs_token，并 Set-Cookie 写入 .10010.com 会话 cookie
//   - 查询：凭登录后捕获的 cookie 直连查询接口（见 api.go），无需浏览器、无需滑块
//
// 参考：GitHub 上 ChinaTelecomOperators/ChinaUnicom 系列青龙脚本（短信通道）。
// 与广电（图片+短信）流程同构：本会话实现 carrier.LoginSession + LoginStateSaver，
// 短信码经 SubmitCode 注入，登录成功后 SaveLogin 回写 cookie/token。

const (
	smsSendURL  = "https://m.client.10010.com/mobileService/sendRadomNum.htm"
	smsLoginURL = "https://m.client.10010.com/mobileService/radomLogin.htm"

	// 联通安全风控返回码
	riskNeedCaptcha = "ECS99998" // 当前账号登录需要图形验证码校验
	riskBlocked     = "ECS99999" // 触发安全风控

	loginUA = "Mozilla/5.0 (Linux; Android 13; LE2100 Build/TP1A.220905.001; wv) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/103.0.5060.129 " +
		"Mobile Safari/537.36; unicom{version:android@10.0100};devicetype{deviceBrand:OnePlus,deviceModel:LE2100};{yw_code:}"

	// 联通标准 RSA 公钥（客户端写死，PKCS#1 v1.5）
	unicomRSAPubPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDc+CZK9bBA9IU+gZUOc6FUGu7y
O9WpTNB0PzmgFBh96Mg1WrovD1oqZ+eIF4LjvxKXGOdI79JRdve9NPhQo07+uqGQ
gE4imwNnRx7PFtCRryiIEcUoavuNtuRVoBAm6qdB0SrctgaqGfLgKvZHOnwTjyNq
jBUxzMeQlEC2czEMSwIDAQAB
-----END PUBLIC KEY-----`
)

// rsaEncrypt 手机号/验证码 RSA(PKCS#1 v1.5) 加密 → base64（与联通客户端一致）
func rsaEncrypt(plain string) (string, error) {
	block, _ := pem.Decode([]byte(unicomRSAPubPEM))
	if block == nil {
		return "", errors.New("联通公钥解析失败")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("联通公钥解析失败: %w", err)
	}
	pubKey, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("联通公钥类型错误")
	}
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pubKey, []byte(plain))
	if err != nil {
		return "", fmt.Errorf("RSA 加密失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// smsSession 联通短信验证码登录会话
type smsSession struct {
	phone     string
	log       *loggerx.Logger
	deviceID  string
	androidID string
	appID     string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	codeCh chan string

	mu        sync.Mutex
	stage     string
	msg       string
	submitted bool
	state     *loginState
}

// loginState 登录成功产物（SaveLogin 回写账号）
type loginState struct {
	Cookie string // 登录后捕获的 .10010.com 会话 cookie（查询用）
	Token  string // token_online
}

// StartLogin 启动联通短信验证码登录会话
func StartLogin(phone string, log *loggerx.Logger) (*smsSession, error) {
	if !store.ValidPhone(phone) {
		return nil, fmt.Errorf("手机号格式不正确")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &smsSession{
		phone:     phone,
		log:       log,
		deviceID:  randHex(32),
		androidID: randHex(16),
		appID:     randHex(64),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		codeCh:    make(chan string, 1),
	}
	go s.run()
	return s, nil
}

func (s *smsSession) run() {
	defer close(s.done)
	s.setStage(carrier.StageCodeSending, "正在向 "+maskPhone(s.phone)+" 发送短信验证码...")
	if err := s.sendCode(); err != nil {
		s.finishError(err)
		return
	}
	s.setStage(carrier.StageWaitingCode, "短信验证码已发送，请输入收到的验证码")
	code, ok := s.waitInput(s.codeCh)
	if !ok {
		s.finishCancelled()
		return
	}
	s.setStage(carrier.StageSubmitting, "正在登录...")
	state, err := s.login(code)
	if err != nil {
		s.finishError(err)
		return
	}
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	s.setStage(carrier.StageSuccess, "")
}

// sendCode 发送短信验证码（sendRadomNum.htm）
func (s *smsSession) sendCode() error {
	body, _, err := postForm(s.ctx, smsSendURL, s.buildForm(""))
	if err != nil {
		return err
	}
	r := gjson.Parse(body)
	if c := r.Get("code").Str; c != "" && c != "0000" {
		return sendCodeErr(c, r)
	}
	return nil
}

// unicomErrText 提取联通错误描述（不同接口字段名不一致：desc / dsc / mainDesc / msg）
func unicomErrText(r gjson.Result) string {
	for _, k := range []string{"desc", "dsc", "mainDesc", "msg"} {
		if v := r.Get(k).Str; v != "" {
			return v
		}
	}
	return ""
}

// sendCodeErr 将联通返回码翻译为可读错误（风控类返回码给出可操作的解除指引）
func sendCodeErr(code string, r gjson.Result) error {
	d := unicomErrText(r)
	switch code {
	case riskNeedCaptcha, riskBlocked:
		if d == "" {
			d = "账号触发安全风控"
		}
		return fmt.Errorf("联通风控拦截(%s)：%s。请关闭 WiFi 改用 4G/5G 流量打开「中国联通」APP 手动登录一次以解除风控，稍后重试", code, d)
	}
	if d != "" {
		return fmt.Errorf("发送验证码失败: %s", d)
	}
	return fmt.Errorf("发送验证码失败( code=%s )", code)
}

// login 用短信验证码登录（radomLogin.htm）
func (s *smsSession) login(code string) (*loginState, error) {
	body, cookies, err := postForm(s.ctx, smsLoginURL, s.buildForm(code))
	if err != nil {
		return nil, err
	}
	r := gjson.Parse(body)
	token := r.Get("token_online").Str
	if token == "" {
		token = r.Get("data.token_online").Str
	}
	ck := cookies
	if ck == "" {
		// 兜底：部分环境未回写 Set-Cookie，仅拿到 token_online
		ck = "token_online=" + token
	}
	if token == "" {
		if d := unicomErrText(r); d != "" {
			return nil, fmt.Errorf("登录失败: %s", d)
		}
		return nil, fmt.Errorf("登录失败：未获取到 token（请确认验证码是否正确）")
	}
	return &loginState{Cookie: ck, Token: token}, nil
}

// buildForm 构造短信发送 / 登录表单（code 为空=发送，非空=登录）
func (s *smsSession) buildForm(code string) url.Values {
	v := url.Values{}
	v.Set("isFirstInstall", "1")
	v.Set("resultToken", "")
	v.Set("provinceCode", "051")
	v.Set("cityCode", "520")
	v.Set("deviceOS", "android13")
	mobile, _ := rsaEncrypt(s.phone)
	v.Set("mobile", mobile)
	v.Set("netWay", "Wifi")
	v.Set("loginCodeLen", "6")
	v.Set("version", "android@10.0600")
	v.Set("deviceCode", s.deviceID)
	v.Set("deviceId", s.deviceID)
	v.Set("pip", "192.168.2.125")
	v.Set("keyVersion", "")
	v.Set("send_flag", "")
	v.Set("provinceChanel", "general")
	v.Set("appId", s.appID)
	v.Set("deviceModel", "V1936A")
	v.Set("androidId", s.androidID)
	v.Set("deviceBrand", "")
	v.Set("timestamp", time.Now().Format("20060102150405"))
	if code != "" {
		v.Set("simCount", "1")
		v.Set("yw_code", "")
		v.Set("loginStyle", "0")
		v.Set("isRemberPwd", "true")
		pwd, _ := rsaEncrypt(code)
		v.Set("password", pwd)
		v.Set("version", "android@10.0100")
	}
	return v
}

// postForm 提交表单（application/x-www-form-urlencoded），返回响应体与捕获的 cookie 串
func postForm(ctx context.Context, rawURL string, form url.Values) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", loginUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh-Hans;q=0.9")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	var cooks []string
	for _, c := range resp.Cookies() {
		cooks = append(cooks, c.Name+"="+c.Value)
	}
	return string(b), strings.Join(cooks, "; "), nil
}

// ---------- carrier.LoginSession ----------

func (s *smsSession) SubmitCode(code string) error {
	if strings.TrimSpace(code) == "" {
		return fmt.Errorf("请输入短信验证码")
	}
	select {
	case s.codeCh <- code:
		s.mu.Lock()
		s.submitted = true
		s.mu.Unlock()
		return nil
	case <-s.ctx.Done():
		return fmt.Errorf("登录会话已结束")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("当前不在验证码输入阶段，请稍后再试")
	}
}

func (s *smsSession) Cancel() { s.cancel() }

func (s *smsSession) Done() <-chan struct{} { return s.done }

func (s *smsSession) Status() (stage, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, s.msg
}

func (s *smsSession) CodeSubmitted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submitted
}

// ---------- carrier.LoginStateSaver ----------

// SaveLogin 会话成功后由 Web 层调用：回写会话 cookie 与 token_online
func (s *smsSession) SaveLogin(a *store.Account) {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state == nil {
		return
	}
	a.Cookie = state.Cookie
	a.Token = state.Token
}

// ---------- 内部 ----------

func (s *smsSession) waitInput(ch chan string) (string, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-s.ctx.Done():
		return "", false
	}
}

func (s *smsSession) setStage(stage, msg string) {
	s.mu.Lock()
	s.stage = stage
	s.msg = msg
	s.mu.Unlock()
}

func (s *smsSession) finishError(err error) {
	if s.log != nil {
		s.log.Error("[%s] 联通登录失败: %v", s.phone, err)
	}
	s.setStage(carrier.StageError, err.Error())
}

func (s *smsSession) finishCancelled() {
	s.setStage(carrier.StageClosed, "已取消")
}

func maskPhone(p string) string {
	if len(p) != 11 {
		return p
	}
	return p[:3] + "****" + p[7:]
}
