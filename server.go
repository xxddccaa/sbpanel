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
	"net/url"
	"strings"
	"time"
)

//go:embed web
var webFiles embed.FS

const (
	csrfHeader      = "X-SBPanel"
	maxRequestBytes = 1 << 20
	actionTimeout   = 2 * time.Minute
)

type Server struct {
	app *App
	// Host names the panel answers to besides IP addresses and localhost
	// (SBPANEL_ALLOWED_HOSTS). "*" turns the check off.
	allowedHosts []string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", noCache(http.FileServer(http.FS(static))))

	routes := map[string]http.HandlerFunc{
		"GET /api/health":                     s.handleHealth,
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
		"GET /api/panel":                      s.handlePanel,
		"POST /api/panel/check":               s.handlePanelCheck,
		"POST /api/panel/install":             s.handlePanelInstall,
		"POST /api/panel/rollback":            s.handlePanelRollback,
		"GET /api/config":                     s.handleConfig,
	}
	for pattern, handler := range routes {
		mux.Handle(pattern, protectRequest(handler))
	}
	return securityHeaders(s.checkHost(mux))
}

// checkHost defends against DNS rebinding. The panel has no password, so without it
// any web page could point its own domain at this server and use the panel as a
// same-origin site. Browsers put the name they connected to in Host, so only IP
// addresses, localhost and explicitly allowed names are served.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, s.allowedHosts) {
			host := requestHostname(r.Host)
			http.Error(w, "sbpanel 不接受用 "+host+" 访问（防止 DNS 重绑定攻击）。\n"+
				"请用 IP 地址打开面板，或者在 sbpanel.env 里加一行 SBPANEL_ALLOWED_HOSTS="+host+" 后运行 sbpanel restart。",
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestHostname(hostport string) string {
	host := hostport
	if parsed, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsed
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

func hostAllowed(hostport string, allowed []string) bool {
	host := requestHostname(hostport)
	if host == "" || net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	for _, name := range allowed {
		if name == "*" || name == host {
			return true
		}
	}
	return false
}

// parseAllowedHosts reads a comma- or space-separated list of host names.
func parseAllowedHosts(value string) []string {
	var hosts []string
	for _, field := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if host := requestHostname(field); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
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

func protectRequest(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "不允许跨站请求")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, r.Host) {
				writeError(w, http.StatusForbidden, "不允许跨站请求")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "请求缺少校验头")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
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

func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.panel.View())
}

func (s *Server) handlePanelCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := actionContext(r)
	defer cancel()
	if err := s.app.CheckPanelRelease(ctx); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.app.panel.View())
}

func (s *Server) handlePanelInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tag string `json:"tag"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	respond(w, s.app.StartPanelInstall(body.Tag))
}

func (s *Server) handlePanelRollback(w http.ResponseWriter, r *http.Request) {
	respond(w, s.app.RollbackPanel())
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
