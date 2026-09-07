package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"chinamobile-monitor/internal/mobile"
	"chinamobile-monitor/internal/push"
	"chinamobile-monitor/internal/store"
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
	Remark        string             `json:"remark"`
	HasLoginState bool               `json:"has_login_state"`
	LastQuery     string             `json:"last_query"`
	LastOK        bool               `json:"last_ok"`
	LastError     string             `json:"last_error"`
	Lines         []string           `json:"lines"`
	Result        *store.QueryResult `json:"result,omitempty"`
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		settings := s.st.GetSettings()
		out := []*accountJSON{}
		for _, a := range s.st.ListAccounts() {
			lines := []string{}
			if a.LastResult != nil {
				lines = mobile.FormatResultLines(a.Phone, a.LastResult, a.EffectiveFields(settings.Push.Fields))
			}
			lastQuery := ""
			if !a.LastQuery.IsZero() {
				lastQuery = a.LastQuery.Format("2006-01-02 15:04:05")
			}
			out = append(out, &accountJSON{
				Phone:         a.Phone,
				Remark:        a.Remark,
				HasLoginState: a.HasLoginState,
				LastQuery:     lastQuery,
				LastOK:        a.LastOK,
				LastError:     a.LastError,
				Lines:         lines,
				Result:        a.LastResult,
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
		Phone  string `json:"phone"`
		Remark string `json:"remark"`
	}
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	if !store.ValidPhone(req.Phone) {
		s.apiFail(w, "手机号格式不正确")
		return
	}
	if _, err := s.st.UpsertAccount(req.Phone, req.Remark); err != nil {
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
	var req struct{ Phone string `json:"phone"` }
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
	running, startedAt := s.runner.Status()
	out := map[string]interface{}{"ok": true, "running": running}
	if running {
		out["started_at"] = startedAt.Format("2006-01-02 15:04:05")
	}
	writeJSON(w, out)
}

// ---------- 登录会话（添加账号）----------

func (s *Server) apiLoginFlowStart(w http.ResponseWriter, r *http.Request) {
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
	if !store.ValidPhone(req.Phone) {
		s.apiFail(w, "手机号格式不正确")
		return
	}
	if _, err := s.st.UpsertAccount(req.Phone, req.Remark); err != nil {
		s.apiFail(w, err.Error())
		return
	}

	// 同一时间只允许一个登录会话
	s.flowMu.mu.Lock()
	if s.flowMu.flow != nil {
		old := s.flowMu.flow
		old.Cancel()
		s.flowMu.mu.Unlock()
		// 等旧会话浏览器关闭（释放 profile 锁）
		select {
		case <-old.Done():
		case <-time.After(15 * time.Second):
		}
		s.flowMu.mu.Lock()
	}
	s.flowMu.mu.Unlock()

	flow, err := mobile.StartLogin(req.Phone, s.dataDir(), false, s.log)
	if err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.flowMu.mu.Lock()
	s.flowMu.flow = flow
	s.flowMu.lastStage = ""
	s.flowMu.lastMsg = ""
	s.flowMu.lastSubmitted = false
	s.flowMu.mu.Unlock()

	phone := req.Phone
	go func() {
		<-flow.Done()
		stage, msg := flow.Status()
		if stage == mobile.StageSuccess {
			s.st.UpdateAccount(phone, func(a *store.Account) { a.HasLoginState = true })
			s.log.Info("[%s] 登录态已保存", phone)
		}
		s.flowMu.mu.Lock()
		if s.flowMu.flow == flow {
			s.flowMu.flow = nil
		}
		s.flowMu.lastStage = stage
		s.flowMu.lastMsg = msg
		s.flowMu.lastSubmitted = flow.CodeSubmitted()
		s.flowMu.mu.Unlock()
	}()
	s.apiOK(w, nil)
}

func (s *Server) apiLoginFlowStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	s.flowMu.mu.Lock()
	flow := s.flowMu.flow
	lastStage, lastMsg, lastSubmitted := s.flowMu.lastStage, s.flowMu.lastMsg, s.flowMu.lastSubmitted
	s.flowMu.mu.Unlock()
	if flow == nil {
		out := map[string]interface{}{"ok": true, "active": false}
		// 附带最近一次会话的结束阶段，前端据此提示结束原因
		if lastStage != "" {
			out["stage"] = lastStage
			out["stage_text"] = mobile.StageText(lastStage)
			if lastMsg != "" {
				out["msg"] = lastMsg
			}
			out["submitted"] = lastSubmitted
		}
		writeJSON(w, out)
		return
	}
	stage, msg := flow.Status()
	writeJSON(w, map[string]interface{}{
		"ok": true, "active": true, "phone": flow.Phone,
		"stage": stage, "stage_text": mobile.StageText(stage), "msg": msg,
	})
}

func (s *Server) apiLoginFlowCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	var req struct{ Code string `json:"code"` }
	if err := s.decodeBody(r, &req); err != nil {
		s.apiFail(w, "参数错误")
		return
	}
	s.flowMu.mu.Lock()
	flow := s.flowMu.flow
	s.flowMu.mu.Unlock()
	if flow == nil {
		s.apiFail(w, "当前没有进行中的登录会话")
		return
	}
	if err := flow.SubmitCode(req.Code); err != nil {
		s.apiFail(w, err.Error())
		return
	}
	s.apiOK(w, nil)
}

func (s *Server) apiLoginFlowCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	s.flowMu.mu.Lock()
	flow := s.flowMu.flow
	s.flowMu.mu.Unlock()
	if flow != nil {
		flow.Cancel()
	}
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
	if _, err := time.ParseInLocation("15:04", req.QueryTime, time.Local); err != nil {
		s.apiFail(w, "查询时间格式应为 HH:MM")
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
	var req struct{ Phone string `json:"phone"` }
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

// dailyDiff 单条快照与上一次记录的差值（话费=上一次余额-当日余额；流量=当日已用-上一次已用）
type dailyDiff struct {
	fee, flow float64
	hasPrev   bool // 是否存在上一次记录
	feeOK     bool // 两次余额均可解析
}

// buildDiffs 号码 → 日期 → 与上一次记录的差值
func buildDiffs(all []store.DailyRecord) map[string]map[string]dailyDiff {
	asc := map[string][]store.DailyRecord{}
	for i := len(all) - 1; i >= 0; i-- { // all 为日期倒序，反转为升序
		d := all[i]
		asc[d.Phone] = append(asc[d.Phone], d)
	}
	diffs := map[string]map[string]dailyDiff{}
	for phone, recs := range asc {
		m := map[string]dailyDiff{}
		for i := 0; i < len(recs); i++ {
			var dd dailyDiff
			if i > 0 && recs[i].Result != nil && recs[i-1].Result != nil {
				cur, prev := recs[i].Result, recs[i-1].Result
				dd.hasPrev = true
				if validBalance(cur.Balance) && validBalance(prev.Balance) {
					dd.fee = prev.BalanceNum - cur.BalanceNum
					dd.feeOK = true
				}
				dd.flow = flowUsedGB(cur.GeneralFlow) - flowUsedGB(prev.GeneralFlow)
			}
			m[recs[i].Date] = dd
		}
		diffs[phone] = m
	}
	return diffs
}

// apiDailyList 每日汇总列表：日期、号码数量、已用话费、已用流量
func (s *Server) apiDailyList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	all := s.st.ListDaily()
	diffs := buildDiffs(all)

	type dayRow struct {
		Date      string  `json:"date"`
		Count     int     `json:"count"`
		Fee       string  `json:"fee"`
		Flow      string  `json:"flow"`
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
	type rec struct {
		Phone       string           `json:"phone"`
		Remark      string           `json:"remark"`
		Balance     string           `json:"balance"`
		GeneralFlow *store.UsageItem `json:"general_flow,omitempty"`
		Voice       *store.UsageItem `json:"voice,omitempty"`
		QueriedAt   string           `json:"queried_at"`
	}
	var recs []*rec
	for _, d := range s.st.ListDaily() {
		if d.Date != date || d.Result == nil {
			continue
		}
		gf, vf := d.Result.GeneralFlow, d.Result.Voice
		recs = append(recs, &rec{
			Phone: d.Phone, Remark: remarks[d.Phone], Balance: d.Result.Balance,
			GeneralFlow: &gf, Voice: &vf, QueriedAt: d.Result.QueriedAt,
		})
	}
	writeJSON(w, map[string]interface{}{"ok": true, "date": date, "records": recs})
}

// apiDailyByPhone 单号码逐日扣费明细
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
		Date        string `json:"date"`
		Balance     string `json:"balance"`
		Fee         string `json:"fee"`
		Flow        string `json:"flow"`
		RealtimeFee string `json:"realtime_fee"`
	}
	var rows []*row
	for _, d := range mine {
		rw := &row{Date: d.Date, Balance: "—", Fee: "—", Flow: "—"}
		if d.Result != nil {
			rw.Balance = d.Result.Balance
			rw.RealtimeFee = d.Result.RealtimeFee
			if dd := diffs[d.Date]; dd.hasPrev {
				if dd.feeOK {
					rw.Fee = fmt.Sprintf("%.2f", dd.fee)
				}
				rw.Flow = fmt.Sprintf("%.2fGB", dd.flow)
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
