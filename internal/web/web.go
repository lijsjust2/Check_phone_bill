package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"

	"chinamobile-monitor/internal/carrier"
	"chinamobile-monitor/internal/loggerx"
	"chinamobile-monitor/internal/runner"
	"chinamobile-monitor/internal/store"
)

//go:embed templates static
var content embed.FS

var templates = template.Must(template.ParseFS(content, "templates/*.html"))

type session struct {
	username string
	expires  time.Time
}

type pendingCode struct {
	code     string
	expires  time.Time
	attempts int
}

type failRecord struct {
	count    int
	blocked  time.Time
	lastFail time.Time
}

// Server Web 面板
type Server struct {
	st     *store.Store
	log    *loggerx.Logger
	runner *runner.Runner
	port   int
	dir    string

	sessMu   sync.Mutex
	sessions map[string]*session

	faMu    sync.Mutex
	pending map[string]*pendingCode // username → 2FA 验证码

	limitMu  sync.Mutex
	failures map[string]*failRecord // ip → 登录失败计数

	flows *carrier.SessionManager
}

// New 创建 Web 服务
func New(st *store.Store, log *loggerx.Logger, r *runner.Runner, port int, dataDir string) *Server {
	s := &Server{
		st:       st,
		log:      log,
		runner:   r,
		port:     port,
		dir:      dataDir,
		sessions: map[string]*session{},
		pending:  map[string]*pendingCode{},
		failures: map[string]*failRecord{},
	}
	// 登录会话结束回调：成功时保存登录态（浏览器型置 HasLoginState；密码型由 LoginStateSaver 写回 token）
	s.flows = carrier.NewSessionManager(func(m carrier.SessionMeta, sess carrier.LoginSession) {
		if m.Stage != carrier.StageSuccess {
			return
		}
		st.UpdateAccount(m.Phone, func(a *store.Account) {
			a.HasLoginState = true
			if saver, ok := sess.(carrier.LoginStateSaver); ok {
				saver.SaveLogin(a)
			}
		})
		log.Info("[%s] 登录态已保存", m.Phone)
	})
	return s
}

// HashPassword scrypt 派生（N=32768, r=8, p=1）
func HashPassword(password, saltHex string) string {
	salt, _ := hex.DecodeString(saltHex)
	dk, err := scrypt.Key([]byte(password), salt, 32768, 8, 1, 32)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(dk)
}

func verifyPassword(password, saltHex, wantHash string) bool {
	got := HashPassword(password, saltHex)
	return subtle.ConstantTimeCompare([]byte(got), []byte(wantHash)) == 1
}

// Start 启动 HTTP 服务（阻塞）
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// 静态资源
	mux.Handle("/static/", http.FileServer(http.FS(content)))

	// 页面
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/setup", s.handleSetup)
	mux.HandleFunc("/login", s.handleLoginPage)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/accounts", s.authPage(s.handleAccountsPage))
	mux.HandleFunc("/daily", s.authPage(s.handleDailyPage))
	mux.HandleFunc("/settings", s.authPage(s.handleSettingsPage))
	mux.HandleFunc("/logs", s.authPage(s.handleLogsPage))

	// API
	mux.HandleFunc("/api/login/send-code", s.apiSendCode)
	mux.HandleFunc("/api/carriers", s.apiCarriers)
	mux.HandleFunc("/api/accounts", s.apiAccounts)
	mux.HandleFunc("/api/accounts/delete", s.apiAccountDelete)
	mux.HandleFunc("/api/accounts/edit", s.apiAccountEdit)
	mux.HandleFunc("/api/accounts/reorder", s.apiAccountReorder)
	mux.HandleFunc("/api/query/all", s.apiQueryAll)
	mux.HandleFunc("/api/query/one", s.apiQueryOne)
	mux.HandleFunc("/api/query/status", s.apiQueryStatus)
	mux.HandleFunc("/api/login-flow/start", s.apiLoginFlowStart)
	mux.HandleFunc("/api/login-flow/status", s.apiLoginFlowStatus)
	mux.HandleFunc("/api/login-flow/code", s.apiLoginFlowCode)
	mux.HandleFunc("/api/login-flow/image-captcha", s.apiLoginFlowImageCaptcha)
	mux.HandleFunc("/api/login-flow/cancel", s.apiLoginFlowCancel)
	mux.HandleFunc("/api/settings", s.apiSettings)
	mux.HandleFunc("/api/settings/push-test", s.apiPushTest)
	mux.HandleFunc("/api/password", s.apiPassword)
	mux.HandleFunc("/api/logs", s.apiLogs)
	mux.HandleFunc("/api/history", s.apiHistory)
	mux.HandleFunc("/api/daily", s.apiDailyList)
	mux.HandleFunc("/api/daily/date", s.apiDailyByDate)
	mux.HandleFunc("/api/daily/phone", s.apiDailyByPhone)
	mux.HandleFunc("/api/backup/export", s.apiBackupExport)
	mux.HandleFunc("/api/backup/import", s.apiBackupImport)
	mux.HandleFunc("/api/diag/net", s.apiDiagNet)

	addr := fmt.Sprintf(":%d", s.port)
	s.log.Info("Web 面板启动: http://localhost:%d", s.port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           secureHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"frame-src 'self'; "+
				"connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// ---------- 会话管理 ----------

func (s *Server) newSession(username string) (token string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = hex.EncodeToString(b)
	s.sessMu.Lock()
	// 顺带清理过期会话
	now := time.Now()
	for k, v := range s.sessions {
		if v.expires.Before(now) {
			delete(s.sessions, k)
		}
	}
	s.sessions[token] = &session{username: username, expires: now.Add(7 * 24 * time.Hour)}
	s.sessMu.Unlock()
	return token
}

func (s *Server) getSession(r *http.Request) *session {
	c, err := r.Cookie("cm_session")
	if err != nil || c.Value == "" {
		return nil
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess := s.sessions[c.Value]
	if sess == nil || sess.expires.Before(time.Now()) {
		return nil
	}
	// 滑动续期
	if time.Until(sess.expires) < 24*time.Hour {
		sess.expires = time.Now().Add(7 * 24 * time.Hour)
	}
	return sess
}

func (s *Server) dropUserSessions(username string) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	for k, v := range s.sessions {
		if v.username == username {
			delete(s.sessions, k)
		}
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "cm_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
		MaxAge:   7 * 24 * 3600,
	})
}

func cookieSecure() bool { return strings.EqualFold(getEnv("COOKIE_SECURE", "0"), "1") }

func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ---------- CSRF（双提交 cookie）----------

func (s *Server) setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "cm_csrf",
		Value:    token,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
		MaxAge:   7 * 24 * 3600,
	})
}

// checkCSRF 校验 X-CSRF-Token 头与 cookie 一致（防跨站请求）
func (s *Server) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie("cm_csrf")
	if err != nil || c.Value == "" {
		return false
	}
	h := r.Header.Get("X-CSRF-Token")
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(h)) == 1
}

// ---------- 登录限流 ----------

func (s *Server) ipBlocked(ip string) bool {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	rec := s.failures[ip]
	if rec == nil {
		return false
	}
	if !rec.blocked.IsZero() && time.Now().Before(rec.blocked) {
		return true
	}
	// 15 分钟内无失败则清零
	if time.Since(rec.lastFail) > 15*time.Minute {
		delete(s.failures, ip)
	}
	return false
}

func (s *Server) recordFailure(ip string) {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	rec := s.failures[ip]
	if rec == nil {
		rec = &failRecord{}
		s.failures[ip] = rec
	}
	rec.count++
	rec.lastFail = time.Now()
	if rec.count >= 5 {
		rec.blocked = time.Now().Add(15 * time.Minute)
		rec.count = 0
		s.log.Warn("IP %s 连续登录失败，已封禁 15 分钟", ip)
	}
}

func (s *Server) clearFailures(ip string) {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	delete(s.failures, ip)
}

func clientIP(r *http.Request) string {
	// 不信任 XFF，直接取连接对端
	idx := strings.LastIndex(r.RemoteAddr, ":")
	if idx < 0 {
		return r.RemoteAddr
	}
	return r.RemoteAddr[:idx]
}

// ---------- 2FA 验证码 ----------

func randomCode6() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "000000"
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func (s *Server) issue2FA(username string) string {
	code := randomCode6()
	s.faMu.Lock()
	// 清理过期
	now := time.Now()
	for k, v := range s.pending {
		if v.expires.Before(now) {
			delete(s.pending, k)
		}
	}
	s.pending[username] = &pendingCode{code: code, expires: now.Add(5 * time.Minute)}
	s.faMu.Unlock()
	return code
}

func (s *Server) verify2FA(username, code string) bool {
	s.faMu.Lock()
	p := s.pending[username]
	if p == nil {
		s.faMu.Unlock()
		return false
	}
	if p.expires.Before(time.Now()) {
		delete(s.pending, username)
		s.faMu.Unlock()
		return false
	}
	p.attempts++
	if p.attempts > 5 {
		delete(s.pending, username)
		s.faMu.Unlock()
		return false
	}
	ok := subtle.ConstantTimeCompare([]byte(p.code), []byte(code)) == 1
	if ok {
		delete(s.pending, username)
	}
	s.faMu.Unlock()
	return ok
}
