package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed web
var webFiles embed.FS

const (
	sessionCookie   = "sbpanel_session"
	csrfHeader      = "X-SBPanel"
	maxRequestBytes = 1 << 20
	actionTimeout   = 2 * time.Minute
)

type Server struct {
	app     *App
	auth    *Auth
	limiter *loginLimiter
	secure  bool // set the Secure cookie flag (panel served over HTTPS)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", noCache(http.FileServer(http.FS(static))))

	mux.HandleFunc("GET /api/auth", s.handleAuthStatus)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	protected := map[string]http.HandlerFunc{
		"POST /api/password":                  s.handleChangePassword,
		"GET /api/state":                      s.handleState,
		"POST /api/subscriptions":             s.handleAddSubscription,
		"PUT /api/subscriptions/{id}":         s.handleEditSubscription,
		"DELETE /api/subscriptions/{id}":      s.handleDeleteSubscription,
		"POST /api/subscriptions/{id}/update": s.handleUpdateSubscription,
		"POST /api/subscriptions/update-all":  s.handleUpdateAll,
		"POST /api/select":                    s.handleSelect,
		"POST /api/core/{action}":             s.handleCore,
		"POST /api/delay-test":                s.handleDelayTest,
		"PUT /api/settings":                   s.handleSettings,
		"GET /api/logs":                       s.handleLogs,
		"POST /api/rulesets/update":           s.handleUpdateRuleSets,
		"GET /api/kernel":                     s.handleKernel,
		"POST /api/kernel/check":              s.handleKernelCheck,
		"POST /api/kernel/install":            s.handleKernelInstall,
		"POST /api/kernel/rollback":           s.handleKernelRollback,
		"POST /api/kernel/use-system":         s.handleKernelUseSystem,
		"GET /api/config":                     s.handleConfig,
	}
	for pattern, handler := range protected {
		mux.Handle(pattern, s.requireSession(handler))
	}
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// requireSession checks the session cookie. Mutating requests must also carry a custom
// header, which a cross-site page cannot add without a CORS preflight we never allow.
func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "请求缺少校验头")
			return
		}
		next(w, r)
	})
}

func (s *Server) loggedIn(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	return err == nil && s.auth.CheckSession(cookie.Value, time.Now())
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"loggedIn": s.loggedIn(r)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != "1" {
		writeError(w, http.StatusForbidden, "请求缺少校验头")
		return
	}
	client := clientAddress(r)
	now := time.Now()
	if wait := s.limiter.blockedFor(client, now); wait > 0 {
		writeError(w, http.StatusTooManyRequests, "密码错误次数太多，请 "+wait.Round(time.Second).String()+" 后再试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ok := s.auth.Verify(body.Password)
	s.limiter.record(client, ok, now)
	if !ok {
		time.Sleep(500 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "密码不对")
		return
	}
	token, err := s.auth.IssueSession(now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !s.auth.Verify(body.Current) {
		writeError(w, http.StatusBadRequest, "当前密码不对")
		return
	}
	if err := s.auth.SetPassword(body.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Changing the password rotates the session key; give this browser a fresh session.
	token, err := s.auth.IssueSession(time.Now())
	if err == nil {
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secure,
			SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.View(r.Context()))
}

func (s *Server) handleAddSubscription(w http.ResponseWriter, r *http.Request) {
	var input SubscriptionInput
	if !readJSON(w, r, &input) {
		return
	}
	ctx, cancel := actionContext(r)
	defer cancel()
	id, err := s.app.AddSubscription(ctx, input)
	if err != nil && id != "" {
		writeError(w, http.StatusBadGateway, "订阅已添加，但更新失败: "+err.Error())
		return
	}
	respond(w, err)
}

func (s *Server) handleEditSubscription(w http.ResponseWriter, r *http.Request) {
	var input SubscriptionInput
	if !readJSON(w, r, &input) {
		return
	}
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.EditSubscription(ctx, r.PathValue("id"), input))
}

func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.DeleteSubscription(ctx, r.PathValue("id")))
}

func (s *Server) handleUpdateSubscription(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.UpdateSubscription(ctx, r.PathValue("id")))
}

func (s *Server) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	failures := s.app.UpdateAll(ctx)
	if len(failures) == 0 {
		respond(w, nil)
		return
	}
	var lines []string
	for name, message := range failures {
		lines = append(lines, name+": "+message)
	}
	writeError(w, http.StatusBadGateway, strings.Join(lines, "\n"))
}

func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SubID string `json:"subId"`
		Tag   string `json:"tag"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var ref *NodeRef
	if body.Tag != "" {
		ref = &NodeRef{SubID: body.SubID, Tag: body.Tag}
	}
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.Select(ctx, ref))
}

func (s *Server) handleCore(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	switch r.PathValue("action") {
	case "start":
		respond(w, s.app.StartCore(ctx))
	case "stop":
		respond(w, s.app.StopCore(ctx))
	case "restart":
		respond(w, s.app.RestartCore(ctx))
	default:
		writeError(w, http.StatusNotFound, "unknown action")
	}
}

func (s *Server) handleDelayTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.TestDelays(ctx))
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var settings Settings
	if !readJSON(w, r, &settings) {
		return
	}
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.UpdateSettings(ctx, settings))
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"lines": s.app.core.Logs()})
}

func (s *Server) handleUpdateRuleSets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.UpdateRuleSets(ctx))
}

func (s *Server) handleKernel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.kernel.View())
}

func (s *Server) handleKernelCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	if err := s.app.CheckKernelReleases(ctx); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.app.kernel.View())
}

func (s *Server) handleKernelInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tag string `json:"tag"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	respond(w, s.app.StartKernelInstall(body.Tag))
}

func (s *Server) handleKernelRollback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.RollbackKernel(ctx))
}

func (s *Server) handleKernelUseSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	respond(w, s.app.UseSystemKernel(ctx))
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	data, err := s.app.GeneratedConfig()
	if err != nil {
		writeError(w, http.StatusNotFound, "还没有生成配置")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}

// actionContext outlives a closed browser tab: half-applied changes are worse than slow ones.
func actionContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), actionTimeout)
}

func respond(w http.ResponseWriter, err error) {
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err == nil {
		err = json.Unmarshal(data, target)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "请求格式不对")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
