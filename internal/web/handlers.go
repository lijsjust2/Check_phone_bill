package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/netx"
	"chinamobile-monitor/internal/push"
	"chinamobile-monitor/internal/store"
	"chinamobile-monitor/internal/telecom"
)

type pageData struct {
	Title    string
	Username string
	Active   string
	Data     map[string]interface{}
}

func (s *Server) render(w http.ResponseWriter, name string, data *pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("渲染模板 %s 失败: %v", name, err)
	}
}

// ---------- 页面 ----------

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.st.HasUser() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if s.getSession(r) == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/accounts", http.StatusFound)
}

func (s *Server) authPage(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.st.HasUser() {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		if s.getSession(r) == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fn(w, r)
	}
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.st.HasUser() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		confirm := r.FormValue("confirm")
		if len(username) < 2 || len(password) < 6 {
			s.render(w, "setup.html", &pageData{Title: "初始化", Data: map[string]interface{}{"Error": "账号至少 2 位，密码至少 6 位", "FormUsername": username}})
			return
		}
		if password != confirm {
			s.render(w, "setup.html", &pageData{Title: "初始化", Data: map[string]interface{}{"Error": "两次输入的密码不一致", "FormUsername": username}})
			return
		}
		salt := store.RandomHex(16)
		if err := s.st.CreateUser(username, HashPassword(password, salt), salt); err != nil {
			s.render(w, "setup.html", &pageData{Title: "初始化", Data: map[string]interface{}{"Error": err.Error()}})
			return
		}
		s.log.Info("管理员账号 %s 创建成功", username)
		token := s.newSession(username)
		csrf := store.RandomHex(16)
		s.setSessionCookie(w, token)
		s.setCSRFCookie(w, csrf)
		http.Redirect(w, r, "/accounts", http.StatusFound)
		return
	}
	s.render(w, "setup.html", &pageData{Title: "初始化", Data: map[string]interface{}{}})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if !s.st.HasUser() {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		code := strings.TrimSpace(r.FormValue("code"))
		ip := clientIP(r)

		fail := func(msg string) {
			s.recordFailure(ip)
			s.render(w, "login.html", &pageData{Title: "登录", Data: map[string]interface{}{
				"Error": msg, "TwoFaEnabled": s.st.GetSettings().TwoFAEnabled(), "FormUsername": username,
			}})
		}

		if s.ipBlocked(ip) {
			fail("失败次数过多，IP 已被封禁，请 15 分钟后再试")
			return
		}
		u, ok := s.st.GetUser(username)
		if !ok || !verifyPassword(password, u.Salt, u.PasswordHash) {
			fail("账号或密码错误")
			return
		}
		if s.st.GetSettings().TwoFAEnabled() {
			if code == "" {
				fail("请先点击「获取验证码」，再填入收到的验证码")
				return
			}
			if !s.verify2FA(username, code) {
				fail("验证码错误或已过期")
				return
			}
		}
		s.clearFailures(ip)
		token := s.newSession(username)
		csrf := store.RandomHex(16)
		s.setSessionCookie(w, token)
		s.setCSRFCookie(w, csrf)
		s.log.Info("用户 %s 登录成功（IP %s）", username, ip)
		http.Redirect(w, r, "/accounts", http.StatusFound)
		return
	}
	s.render(w, "login.html", &pageData{Title: "登录", Data: map[string]interface{}{
		"TwoFaEnabled": s.st.GetSettings().TwoFAEnabled(),
		"TwoFaChannel": s.st.GetSettings().TwoFA.Channel,
	}})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("cm_session"); err == nil && c.Value != "" {
		s.sessMu.Lock()
		delete(s.sessions, c.Value)
		s.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "cm_session", Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "cm_csrf", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) handleAccountsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "accounts.html", &pageData{Title: "账号管理", Username: s.username(r), Active: "accounts"})
}

func (s *Server) handleDailyPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "daily.html", &pageData{Title: "费用明细", Username: s.username(r), Active: "daily"})
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings.html", &pageData{Title: "设置", Username: s.username(r), Active: "settings",
		Data: map[string]interface{}{"FieldLabels": store.FieldLabels()}})
}

func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "logs.html", &pageData{Title: "日志", Username: s.username(r), Active: "logs"})
}

func (s *Server) username(r *http.Request) string {
	sess := s.getSession(r)
	if sess == nil {
		return ""
	}
	return sess.username
}

// ---------- API 基础 ----------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// validQueryTime 校验 HH:MM 或逗号分隔的多个 HH:MM，至少一项有效即通过
func validQueryTime(s string) bool {
	got := 0
	for _, p := range strings.Split(s, ",") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if _, err := time.ParseInLocation("15:04", strings.TrimSpace(p), time.Local); err == nil {
			got++
		}
	}
	return got > 0
}

func (s *Server) apiFail(w http.ResponseWriter, msg string) {
	writeJSON(w, map[string]interface{}{"ok": false, "error": msg})
}

func (s *Server) apiOK(w http.ResponseWriter, extra map[string]interface{}) {
	out := map[string]interface{}{"ok": true}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, out)
}

// apiAuth API 鉴权：登录 + POST 需 CSRF 校验
func (s *Server) apiAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.st.HasUser() || s.getSession(r) == nil {
			s.apiFail(w, "未登录")
			return
		}
		if r.Method == http.MethodPost && !s.checkCSRF(r) {
			s.apiFail(w, "CSRF 校验失败，请刷新页面重试")
			return
		}
		fn(w, r)
	}
}

func (s *Server) decodeBody(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// ---------- 登录验证码（面板 2FA）----------

func (s *Server) apiSendCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.apiFail(w, "方法不允许")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	ip := clientIP(r)

	if s.ipBlocked(ip) {
		s.apiFail(w, "失败次数过多，IP 已被封禁，请 15 分钟后再试")
		return
	}
	u, ok := s.st.GetUser(username)
	if !ok || !verifyPassword(password, u.Salt, u.PasswordHash) {
		s.recordFailure(ip)
		s.apiFail(w, "账号或密码错误")
		return
	}
	settings := s.st.GetSettings()
	if !settings.TwoFAEnabled() {
		s.apiFail(w, "2FA 未启用，直接登录即可")
		return
	}
	code := s.issue2FA(username)
	if err := push.Send2FACode(settings.Push, settings.TwoFA.Channel, code); err != nil {
		s.log.Error("[%s] 2FA 验证码推送失败: %v", username, err)
		s.apiFail(w, "验证码推送失败: "+err.Error())
		return
	}
	s.log.Info("[%s] 2FA 验证码已推送（渠道 %s，IP %s）", username, settings.TwoFA.Channel, ip)
	s.apiOK(w, nil)
}

// ---------- 账号管理 ----------

type accountJSON struct {
	Phone         string             `json:"phone"`
	Carrier       string             `json:"carrier"`
	Remark        string             `json:"remark"`
	HasLoginState bool               `json:"has_login_state"`
	LastQuery     string             `json:"last_query"`
	LastOK        bool               `json:"last_ok"`
	LastError     string             `json:"last_error"`
	Lines         []string           `json:"lines"`
	Result        *store.QueryResult `json:"result,omitempty"`
	OpenID        string             `json:"openid,omitempty"` // 联通：已配置的微信小程序 OpenID（重新登录时预填）
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settings := s.st.GetSettings()
		out := []*accountJSON{}
		for _, a := range s.st.ListAccounts() {
			lines := []string{}
			if a.LastResult != nil {
				lines = carrier.FormatResultLines(a.Phone, a.LastResult, a.EffectiveFields(settings.Push.Fields))
			}
			lastQuery := ""
			if !a.LastQuery.IsZero() {
				lastQuery = a.LastQuery.Format("2006-01-02 15:04:05")
			}
			out = append(out, &accountJSON{
				Phone:         a.Phone,
				Carrier:       a.CarrierCode(),
				Remark:        a.Remark,
				HasLoginState: a.HasLoginState,
				LastQuery:     lastQuery,
				LastOK:        a.LastOK,
				LastError:     a.LastError,
				Lines:         lines,
				Result:        a.LastResult,
				OpenID:        a.OpenID,
			})
		}
		writeJSON(w, map[string]interface{}{"ok": true, "accounts": out})
		return
	}

	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		s.apiFail(w, "方法不允许")
		return
	}
	var req struct {
		Phone   string `json:"phone"`
		Carrier string `json:"carrier"`
		Remark  string `json:"remark"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if !store.ValidPhone(req.Phone) {
		s.apiFail(w, "手机号格式不正确")
		return
	}
	if _, err := s.st.UpsertAccount(req.Phone, req.Carrier, req.Remark); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.apiOK(w, nil)
}

// ---------- 查询 ----------

func (s *Server) apiQueryAll(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if err := s.runner.QueryAll(false); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.apiOK(w, nil)
}

func (s *Server) apiQueryOne(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if err := s.decodeBody(r, &req); err != nil || req.Phone == "" {
		s.apiFail(w, "参数错误")
		return
	}
	if s.st.GetAccount(req.Phone) == nil {
		s.apiFail(w, "账号不存在")
		return
	}
	go func() {
		if _, err := s.runner.QueryOne(req.Phone); err != nil {
			s.log.Error("[%s] 查询失败: %v", req.Phone, err)
		}
	}()
	s.apiOK(w, nil)
}

func (s *Server) apiQueryStatus(w http.ResponseWriter, r *http.Request) {
	running, startedAt, currentPhone := s.runner.Status()
	out := map[string]interface{}{"ok": true, "running": running}
	if running {
		out["started_at"] = startedAt.Format("2006-01-02 15:04:05")
		if currentPhone != "" {
			out["current_phone"] = currentPhone
		}
	}
	writeJSON(w, out)
}

// ---------- 登录会话（添加账号）----------

// apiCarriers 已注册运营商列表（前端添加账号弹窗数据源）
func (s *Server) apiCarriers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	type carrierInfo struct {
		Code          string `json:"code"`
		Name          string `json:"name"`
		NeedsPassword bool   `json:"needs_password"`
		NeedsSMSCode  bool   `json:"needs_sms_code"`
		NeedsOpenID   bool   `json:"needs_openid"`
	}
	out := []carrierInfo{}
	for _, p := range carrier.All() {
		out = append(out, carrierInfo{
			Code: p.Code(), Name: p.Name(),
			NeedsPassword: p.NeedsPassword(), NeedsSMSCode: p.NeedsSMSCode(),
			NeedsOpenID: p.NeedsOpenID(),
		})
	}
	writeJSON(w, map[string]interface{}{"ok": true, "carriers": out})
}

func (s *Server) apiLoginFlowStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Phone    string `json:"phone"`
		Carrier  string `json:"carrier"`
		Remark   string `json:"remark"`
		Password string `json:"password"` // 电信服务密码
		OpenID   string `json:"openid"`   // 联通微信小程序 OpenID
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if !store.ValidPhone(req.Phone) {
		s.apiFail(w, "手机号格式不正确")
		return
	}
	p := carrier.Get(req.Carrier)
	if p.Code() != req.Carrier {
		s.apiFail(w, "不支持的运营商")
		return
	}
	if p.NeedsPassword() && strings.TrimSpace(req.Password) == "" {
		s.apiFail(w, "请填写服务密码")
		return
	}
	if p.NeedsOpenID() && strings.TrimSpace(req.OpenID) == "" {
		s.apiFail(w, "请填写微信小程序 OpenID")
		return
	}
	if _, err := s.st.UpsertAccount(req.Phone, req.Carrier, req.Remark); err != nil {
		s.apiFail(w, err.Error())
		return
	}

	// 电信重新登录：复用已绑定设备 androidId，避免重复短信验证
	androidID := ""
	if acc := s.st.GetAccount(req.Phone); acc != nil {
		androidID = acc.AndroidID
	}

	if err := s.flows.Start(p, carrier.LoginParams{
		Phone:     req.Phone,
		Password:  strings.TrimSpace(req.Password),
		OpenID:    strings.TrimSpace(req.OpenID),
		AndroidID: androidID,
		DataDir:   s.dataDir(),
		Headless:  true, // 联通 OpenID 登录为纯 HTTP，所有运营商全程无头
		Log:       s.log,
	}); err != nil {
		s.log.Error("[%s] %s登录流程启动失败: %v", req.Phone, p.Name(), err)
		s.apiFail(w, err.Error())
		return
	}
	s.log.Info("[%s] 启动 %s 登录流程", req.Phone, p.Name())
	s.apiOK(w, nil)
}

func (s *Server) apiLoginFlowStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	active, meta, sess := s.flows.StatusDetail()
	if !active {
		out := map[string]interface{}{"ok": true, "active": false}
		// 附带最近一次会话的结束阶段，前端据此提示结束原因
		if meta.Stage != "" {
			out["stage"] = meta.Stage
			out["stage_text"] = carrier.StageText(meta.Stage)
			if meta.Msg != "" {
				out["msg"] = meta.Msg
			}
			out["submitted"] = meta.Submitted
		}
		writeJSON(w, out)
		return
	}
	out := map[string]interface{}{
		"ok": true, "active": true, "phone": meta.Phone, "carrier": meta.Carrier,
		"stage": meta.Stage, "stage_text": carrier.StageText(meta.Stage), "msg": meta.Msg,
	}
	// 电信设备注册：need_image_captcha 阶段附带图片验证码（data URI）
	if meta.Stage == carrier.StageNeedImageCaptcha {
		if ics, ok := sess.(carrier.ImageCaptchaSession); ok {
			if img := ics.CaptchaImage(); img != "" {
				out["captcha_image"] = img
			}
		}
	}
	writeJSON(w, out)
}

func (s *Server) apiLoginFlowCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if err := s.flows.SubmitCode(req.Code); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.apiOK(w, nil)
}

// apiLoginFlowImageCaptcha 电信图片验证码提交（设备注册流程）
func (s *Server) apiLoginFlowImageCaptcha(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Captcha string `json:"captcha"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if err := s.flows.SubmitImageCaptcha(strings.TrimSpace(req.Captcha)); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.apiOK(w, nil)
}

func (s *Server) apiLoginFlowCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	s.flows.Cancel()
	s.apiOK(w, nil)
}

// ---------- 设置 ----------

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]interface{}{"ok": true, "settings": s.st.GetSettings()})
		return
	}
	var req store.Settings
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	// 查询时间：HH:MM，单个时间即可（如 "08:00"），也可填多个用逗号分隔
	if !validQueryTime(req.QueryTime) {
		s.apiFail(w, "查询时间格式应为 HH:MM，如 08:00；多个时间用英文逗号分隔")
		return
	}
	if req.Push.BarkEnabled && strings.TrimSpace(req.Push.BarkKey) == "" {
		s.apiFail(w, "已勾选启用 Bark，请填写 Bark Key")
		return
	}
	if req.Push.PushPlusEnabled && strings.TrimSpace(req.Push.PushPlusToken) == "" {
		s.apiFail(w, "已勾选启用 PushPlus，请填写 PushPlus Token")
		return
	}
	// 2FA 渠道：none / bark / pushplus 三选一
	channel := req.TwoFA.Channel
	if channel == "" {
		channel = "none"
	}
	if channel != "none" && channel != "bark" && channel != "pushplus" {
		s.apiFail(w, "2FA 渠道参数不正确")
		return
	}
	if channel == "bark" && !(req.Push.BarkEnabled && strings.TrimSpace(req.Push.BarkKey) != "") {
		s.apiFail(w, "选择通过 Bark 推送验证码，需先启用 Bark 并填写 Key")
		return
	}
	if channel == "pushplus" && !(req.Push.PushPlusEnabled && strings.TrimSpace(req.Push.PushPlusToken) != "") {
		s.apiFail(w, "选择通过 PushPlus 推送验证码，需先启用 PushPlus 并填写 Token")
		return
	}
	if req.Push.AlertFlowPercent < 0 || req.Push.AlertFlowPercent > 100 {
		s.apiFail(w, "流量告警百分比应在 0-100 之间")
		return
	}
	old := s.st.GetSettings()
	err := s.st.UpdateSettings(func(cur *store.Settings) {
		cur.QueryTime = req.QueryTime
		cur.Push = req.Push
		cur.TwoFA.Channel = channel
	})
	if err != nil {
		s.apiFail(w, err.Error())
		return
	}
	if old.TwoFA.Channel != channel {
		channelText := map[string]string{"none": "关闭", "bark": "启用（Bark）", "pushplus": "启用（PushPlus）"}[channel]
		s.log.Info("2FA 已%v", channelText)
	}
	s.log.Info("设置已更新（查询时间 %s）", req.QueryTime)
	s.apiOK(w, nil)
}

func (s *Server) apiPushTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if err := push.SendTest(s.st.GetSettings().Push); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.apiOK(w, nil)
}

func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	username := s.username(r)
	u, _ := s.st.GetUser(username)
	if !verifyPassword(req.Old, u.Salt, u.PasswordHash) {
		s.apiFail(w, "原密码错误")
		return
	}
	if len(req.New) < 6 {
		s.apiFail(w, "新密码至少 6 位")
		return
	}
	salt := store.RandomHex(16)
	if err := s.st.UpdatePassword(username, HashPassword(req.New, salt), salt); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	// 吊销其他会话（保留当前）
	if c, err := r.Cookie("cm_session"); err == nil {
		current := c.Value
		s.sessMu.Lock()
		for k, v := range s.sessions {
			if v.username == username && k != current {
				delete(s.sessions, k)
			}
		}
		s.sessMu.Unlock()
	}
	s.log.Info("[%s] 密码已修改", username)
	s.apiOK(w, nil)
}

// ---------- 账号编辑 / 删除 ----------

func (s *Server) apiAccountReorder(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Phone string `json:"phone"`
		Dir   string `json:"dir"` // "up" | "down"
	}
	if err := s.decodeBody(r, &req); err != nil || (req.Dir != "up" && req.Dir != "down") {
		s.apiFail(w, "参数错误")
		return
	}
	if s.st.GetAccount(req.Phone) == nil {
		s.apiFail(w, "账号不存在")
		return
	}
	if !s.st.MoveAccountOrder(req.Phone, req.Dir) {
		s.apiFail(w, "已在边缘位置或保存失败")
		return
	}
	s.apiOK(w, nil)
}

func (s *Server) apiAccountEdit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Phone  string `json:"phone"`
		Remark string `json:"remark"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if s.st.GetAccount(req.Phone) == nil {
		s.apiFail(w, "账号不存在")
		return
	}
	s.st.UpdateAccount(req.Phone, func(a *store.Account) {
		a.Remark = req.Remark
	})
	s.apiOK(w, nil)
}

func (s *Server) apiAccountDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct {
		Phone string `json:"phone"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if !s.st.DeleteAccount(req.Phone) {
		s.apiFail(w, "账号不存在")
		return
	}
	// 同时删除登录态与历史查询数据
	_ = os.RemoveAll(store.AccountsDir(s.dataDir(), req.Phone))
	s.log.Info("[%s] 账号已删除（含登录态数据）", req.Phone)
	s.apiOK(w, nil)
}

// ---------- 日志 ----------

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "entries": s.log.Entries(300)})
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "history": s.st.ListHistory()})
}

// ---------- 费用明细（每日快照） ----------

// validBalance 余额是否为可解析的数值（"未知"等页面兜底文本不可用）
func validBalance(b string) bool { return b != "" && !strings.Contains(b, "未知") }

// flowUsedGB 流量已用量统一换算为 GB（unit "03"=MB，其余按 GB）
func flowUsedGB(u store.UsageItem) float64 {
	if u.Unit == "03" {
		return u.UsedNum / 1024
	}
	return u.UsedNum
}

// dailyDiff 今日查询与昨日查询的差值（今日 − 昨日）
// 扣款 = 今日余额 − 昨日余额（负数=扣费/支出，正数=充值/退款）；流量/语音 = 今日已用 − 昨日已用
type dailyDiff struct {
	fee, flow, voice float64
	hasPrev          bool // 是否存在上一次记录
	feeOK            bool // 两次余额均可解析
}

// buildDiffs 号码 → 日期 → 差值（今日查询 − 昨日查询，银行账单口径：支出为负、收入为正）
func buildDiffs(all []store.DailyRecord) map[string]map[string]dailyDiff {
	byPhone := map[string][]store.DailyRecord{}
	for _, d := range all {
		byPhone[d.Phone] = append(byPhone[d.Phone], d)
	}
	diffs := map[string]map[string]dailyDiff{}
	for phone, recs := range byPhone {
		sort.Slice(recs, func(i, j int) bool { return recs[i].Date < recs[j].Date }) // 按日期升序，确保 cur 为新记录
		m := map[string]dailyDiff{}
		for i := 0; i < len(recs); i++ {
			var dd dailyDiff
			if i > 0 && recs[i].Result != nil && recs[i-1].Result != nil {
				cur, prev := recs[i].Result, recs[i-1].Result
				dd.hasPrev = true
				if validBalance(cur.Balance) && validBalance(prev.Balance) {
					dd.fee = cur.BalanceNum - prev.BalanceNum
					dd.feeOK = true
				}
				dd.flow = flowUsedGB(cur.GeneralFlow) - flowUsedGB(prev.GeneralFlow)
				dd.voice = cur.Voice.UsedNum - prev.Voice.UsedNum
			}
			m[recs[i].Date] = dd
		}
		diffs[phone] = m
	}
	return diffs
}

// apiDailyList 每日汇总列表：日期、号码数量、昨日扣款、昨日流量（各号码差值之和）
func (s *Server) apiDailyList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	all := s.st.ListDaily()
	diffs := buildDiffs(all)

	type dayRow struct {
		Date      string `json:"date"`
		Count     int    `json:"count"`
		Fee       string `json:"fee"`
		Flow      string `json:"flow"`
		feeSum    float64
		feeCount  int
		flowSum   float64
		flowCount int
	}
	seen := map[string]*dayRow{}
	var days []*dayRow
	for _, d := range all {
		row, ok := seen[d.Date]
		if !ok {
			row = &dayRow{Date: d.Date}
			seen[d.Date] = row
			days = append(days, row)
		}
		row.Count++
		if dd := diffs[d.Phone][d.Date]; dd.hasPrev {
			if dd.feeOK {
				row.feeSum += dd.fee
				row.feeCount++
			}
			row.flowSum += dd.flow
			row.flowCount++
		}
	}
	for _, row := range days {
		if row.feeCount > 0 {
			row.Fee = fmt.Sprintf("%.2f", row.feeSum)
		} else {
			row.Fee = "—"
		}
		if row.flowCount > 0 {
			row.Flow = fmt.Sprintf("%.2fGB", row.flowSum)
		} else {
			row.Flow = "—"
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "days": days})
}

// apiDailyByDate 某日各号码快照（列表信息与账号管理一致）
func (s *Server) apiDailyByDate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	date := r.URL.Query().Get("date")
	if _, err := time.Parse("2006-01-02", date); err != nil {
		s.apiFail(w, "日期格式不正确")
		return
	}
	remarks := map[string]string{}
	for _, a := range s.st.ListAccounts() {
		remarks[a.Phone] = a.Remark
	}
	all := s.st.ListDaily()
	diffs := buildDiffs(all)
	type rec struct {
		Phone     string `json:"phone"`
		Remark    string `json:"remark"`
		Balance   string `json:"balance"`
		Fee       string `json:"fee"`   // 昨日扣款（元）：今日余额 − 昨日余额（负数=扣费，正数=充值/退款）
		Flow      string `json:"flow"`  // 昨日流量增量（GB）：今日已用 − 昨日已用
		Voice     string `json:"voice"` // 昨日语音增量（分钟）：今日已用 − 昨日已用
		QueriedAt string `json:"queried_at"`
	}
	var recs []*rec
	for _, d := range all {
		if d.Date != date || d.Result == nil {
			continue
		}
		rw := &rec{
			Phone: d.Phone, Remark: remarks[d.Phone], Balance: d.Result.Balance, QueriedAt: d.Result.QueriedAt,
		}
		if dd := diffs[d.Phone][d.Date]; dd.hasPrev {
			if dd.feeOK {
				rw.Fee = fmt.Sprintf("%.2f", dd.fee)
			}
			rw.Flow = fmt.Sprintf("%.2fGB", dd.flow)
			rw.Voice = fmt.Sprintf("%.0f分钟", dd.voice)
		}
		recs = append(recs, rw)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "date": date, "records": recs})
}

// apiDailyByPhone 单号码逐日完整明细：每日快照全量字段 + 与前一日差值
func (s *Server) apiDailyByPhone(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	phone := r.URL.Query().Get("phone")
	if !store.ValidPhone(phone) {
		s.apiFail(w, "手机号格式不正确")
		return
	}
	remark := ""
	if a := s.st.GetAccount(phone); a != nil {
		remark = a.Remark
	}
	all := s.st.ListDaily()
	var mine []store.DailyRecord
	for _, d := range all {
		if d.Phone == phone {
			mine = append(mine, d)
		}
	}
	diffs := buildDiffs(all)[phone]

	type row struct {
		Date         string           `json:"date"`
		Fee          string           `json:"fee"`        // 昨日扣款（元）：今日余额 − 昨日余额
		Flow         string           `json:"flow"`       // 昨日流量增量（GB）：今日已用 − 昨日已用
		VoiceDiff    string           `json:"voice_diff"` // 昨日语音增量（分钟）
		Balance      string           `json:"balance"`
		RealtimeFee  string           `json:"realtime_fee"`
		GeneralFlow  *store.UsageItem `json:"general_flow,omitempty"`
		SpecialFlow  *store.UsageItem `json:"special_flow,omitempty"`
		RegionalFlow *store.UsageItem `json:"regional_flow,omitempty"`
		Voice        *store.UsageItem `json:"voice,omitempty"`
		Sms          *store.UsageItem `json:"sms,omitempty"`
	}
	var rows []*row
	for _, d := range mine {
		rw := &row{Date: d.Date, Balance: "—", Fee: "—", Flow: "—", VoiceDiff: "—"}
		if d.Result != nil {
			rw.Balance = d.Result.Balance
			rw.RealtimeFee = d.Result.RealtimeFee
			gf, sf, rf := d.Result.GeneralFlow, d.Result.SpecialFlow, d.Result.RegionalFlow
			vv, sv := d.Result.Voice, d.Result.Sms
			rw.GeneralFlow, rw.SpecialFlow, rw.RegionalFlow = &gf, &sf, &rf
			rw.Voice, rw.Sms = &vv, &sv
			if dd := diffs[d.Date]; dd.hasPrev {
				if dd.feeOK {
					rw.Fee = fmt.Sprintf("%.2f", dd.fee)
				}
				rw.Flow = fmt.Sprintf("%.2fGB", dd.flow)
				rw.VoiceDiff = fmt.Sprintf("%.0f分钟", dd.voice)
			}
		}
		rows = append(rows, rw)
	}
	writeJSON(w, map[string]interface{}{"ok": true, "phone": phone, "remark": remark, "records": rows})
}

// ---------- 工具 ----------

// requireAPI 登录 + CSRF（POST）
func (s *Server) requireAPI(w http.ResponseWriter, r *http.Request) bool {
	if !s.st.HasUser() || s.getSession(r) == nil {
		s.apiFail(w, "未登录")
		return false
	}
	if r.Method == http.MethodPost && !s.checkCSRF(r) {
		s.apiFail(w, "CSRF 校验失败，请刷新页面重试")
		return false
	}
	return true
}

// dataDir 数据目录（Server 初始化时保存）
func (s *Server) dataDir() string { return s.dir }

// apiDiagNet 网络诊断：从容器内分阶段（DNS→TCP→TLS→HTTP）探测各运营商接口连通性。
// 排查 Docker 环境（飞牛OS 等）出口网络问题用：登录面板后在浏览器直接访问本接口。
// 探测走与生产一致的出口配置（强制 IPv4 + MSS 钳制 + 电信兼容 TLS）。
func (s *Server) apiDiagNet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	type target struct {
		name, url, method string
		bodyLen           int
	}
	targets := []target{
		{"基线·百度", "https://www.baidu.com/", http.MethodGet, 0},
		{"大包POST·MTU探测", "https://www.baidu.com/", http.MethodPost, 2048},
		{"电信登录网关", "https://appgologin.189.cn:9031/login/client/userLoginNormal", http.MethodGet, 0},
		{"电信查询网关", "https://appfuwu.189.cn:9021/query/qryImportantData", http.MethodGet, 0},
		{"联通小程序网关", "https://mina.10010.com/wxapplet/weixinNew/getTicket", http.MethodGet, 0},
		{"联通掌厅接口", "https://mxx.client.10010.com/servicebusiness/wx/serviceEntrance", http.MethodGet, 0},
		{"广电营业厅", "https://www.10099.com.cn/login.html", http.MethodGet, 0},
	}
	type result struct {
		Name       string   `json:"name"`
		URL        string   `json:"url"`
		Method     string   `json:"method,omitempty"`
		IPs        []string `json:"ips,omitempty"`
		DNSMs      int64    `json:"dns_ms,omitempty"`
		TCPMs      int64    `json:"tcp_ms,omitempty"`
		TLSMs      int64    `json:"tls_ms,omitempty"`
		TLSVersion string   `json:"tls_version,omitempty"`
		HTTPStatus int      `json:"http_status,omitempty"`
		HTTPMs     int64    `json:"http_ms,omitempty"`
		Phase      string   `json:"phase,omitempty"` // 失败阶段：dns/tcp/tls/http
		Error      string   `json:"error,omitempty"`
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			res := result{Name: t.name, URL: t.url, Method: t.method}
			defer func() { results[i] = res }()
			var dnsAt, tcpAt, tlsAt, httpAt time.Time
			trace := &httptrace.ClientTrace{
				DNSDone: func(info httptrace.DNSDoneInfo) {
					if dnsAt.IsZero() {
						dnsAt = time.Now()
					}
					for _, a := range info.Addrs {
						res.IPs = append(res.IPs, a.String())
					}
				},
				ConnectDone: func(_, _ string, err error) {
					if err == nil && tcpAt.IsZero() {
						tcpAt = time.Now()
					}
				},
				TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
					if err == nil && tlsAt.IsZero() {
						tlsAt = time.Now()
						res.TLSVersion = tls.VersionName(cs.Version)
					}
				},
				GotFirstResponseByte: func() {
					if httpAt.IsZero() {
						httpAt = time.Now()
					}
				},
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			ctx = httptrace.WithClientTrace(ctx, trace)
			var body io.Reader
			if t.bodyLen > 0 {
				body = strings.NewReader(strings.Repeat("x", t.bodyLen))
			}
			req, err := http.NewRequestWithContext(ctx, t.method, t.url, body)
			if err != nil {
				res.Phase, res.Error = "http", err.Error()
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36")
			client := &http.Client{
				Timeout: 15 * time.Second,
				Transport: &http.Transport{
					DialContext:           netx.IPv4DialContext,
					TLSClientConfig:       telecom.TLSConfig(),
					TLSHandshakeTimeout:   12 * time.Second,
					ResponseHeaderTimeout: 12 * time.Second,
				},
			}
			t0 := time.Now()
			resp, err := client.Do(req)
			// 各阶段耗时：本阶段起点 = 上一阶段完成时刻（缺失时回退 t0）
			prev := t0
			if !dnsAt.IsZero() {
				res.DNSMs = spanMs(prev, dnsAt)
				prev = dnsAt
			}
			if !tcpAt.IsZero() {
				res.TCPMs = spanMs(prev, tcpAt)
				prev = tcpAt
			}
			if !tlsAt.IsZero() {
				res.TLSMs = spanMs(prev, tlsAt)
				prev = tlsAt
			}
			if !httpAt.IsZero() {
				res.HTTPMs = spanMs(prev, httpAt)
			}
			if err != nil {
				res.Phase, res.Error = netErrPhase(err), err.Error()
				return
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			res.HTTPStatus = resp.StatusCode
		}(i, t)
	}
	wg.Wait()
	writeJSON(w, map[string]interface{}{"ok": true, "results": results})
}

// spanMs 计算两个时间点之间的毫秒数（无效输入返回 0）
func spanMs(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0
	}
	return to.Sub(from).Milliseconds()
}

// netErrPhase 按错误文本粗分失败阶段
func netErrPhase(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "lookup"):
		return "dns"
	case strings.Contains(s, "dial"):
		return "tcp"
	case strings.Contains(s, "TLS"):
		return "tls"
	default:
		return "http"
	}
}
