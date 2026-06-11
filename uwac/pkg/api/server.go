// Package api exposes uwacd over HTTP: the agent-DID auth lane, the OAuth
// connect/callback, and the tool invoke endpoint. Transport auth is a shared
// bearer (MATRIX_UWAC_TOKEN); principal auth is the X-UWAC-Agent token minted by
// the verify lane and resolved to an owner user id.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/paxlabs-inc/uwac/internal/engine"
	"github.com/paxlabs-inc/uwac/pkg/types"
)

// Server wraps the engine with an HTTP mux.
type Server struct {
	eng    *engine.Engine
	log    *slog.Logger
	mux    *http.ServeMux
	server *http.Server
	auth   string
}

// New constructs the API server. auth is the shared transport bearer.
func New(eng *engine.Engine, auth string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{eng: eng, log: log, mux: http.NewServeMux(), auth: auth}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /", s.handleRoot)
	s.mux.HandleFunc("POST /v1/agent/auth/challenge", s.postChallenge)
	s.mux.HandleFunc("POST /v1/agent/auth/verify", s.postVerify)
	s.mux.HandleFunc("POST /v1/invoke", s.postInvoke)
	s.mux.HandleFunc("POST /v1/connect", s.postConnect)
	s.mux.HandleFunc("GET /v1/connect/callback", s.getCallback)
	s.mux.HandleFunc("POST /v1/disconnect", s.postDisconnect)
	return s
}

// ListenAndServe starts HTTP.
func (s *Server) ListenAndServe(addr string) error {
	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.authMiddleware(s.mux),
		ReadHeaderTimeout: 30 * time.Second,
	}
	if s.auth == "" {
		s.log.Warn("uwacd: no MATRIX_UWAC_TOKEN set — transport auth disabled; bind to loopback/6PN only")
	}
	s.log.Info("uwacd listening", "addr", addr)
	return s.server.ListenAndServe()
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// authMiddleware enforces the shared transport bearer on all paths except the
// public ones (healthz, root, and the browser-facing OAuth callback, which is
// validated by its signed state token).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == "" || isPublicPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.auth)) != 1 {
			writeJSON(w, http.StatusUnauthorized, types.Fail(types.NewError(types.CodeUnauthorized, "missing or invalid transport bearer", false)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicPath(r *http.Request) bool {
	if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/healthz") {
		return true
	}
	// The OAuth provider redirects the user's browser here with no bearer; the
	// signed state token is the integrity check.
	return r.Method == http.MethodGet && r.URL.Path == "/v1/connect/callback"
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]string{
		"service": "uwacd",
		"version": engine.Version,
		"health":  "/healthz",
	}))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"status": "ok", "version": engine.Version}))
}

func (s *Server) postChallenge(w http.ResponseWriter, r *http.Request) {
	var req types.ChallengeRequest
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.eng.Challenge(req.DID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, types.Fail(types.NewError(types.CodeInvalidRequest, err.Error(), false)))
		return
	}
	writeJSON(w, http.StatusOK, types.OK(resp))
}

func (s *Server) postVerify(w http.ResponseWriter, r *http.Request) {
	var req types.VerifyRequest
	if !decode(w, r, &req) {
		return
	}
	resp, err := s.eng.Verify(req)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, types.Fail(types.NewError(types.CodeUnauthorized, err.Error(), false)))
		return
	}
	writeJSON(w, http.StatusOK, types.OK(resp))
}

func (s *Server) postInvoke(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req types.InvokeRequest
	if !decode(w, r, &req) {
		return
	}
	env := s.eng.Invoke(r.Context(), owner, req)
	status := http.StatusOK
	if !env.Ok {
		status = statusFor(env.Error)
	}
	writeJSON(w, status, env)
}

func (s *Server) postConnect(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Connector string `json:"connector"`
	}
	if !decode(w, r, &req) {
		return
	}
	authURL, err := s.eng.Connect(owner, req.Connector)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, types.Fail(types.NewError(types.CodeInvalidRequest, err.Error(), false)))
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]string{"authorize_url": authURL}))
}

func (s *Server) getCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		writeHTML(w, http.StatusBadRequest, "Connection failed: missing state or code.")
		return
	}
	if err := s.eng.Callback(r.Context(), state, code); err != nil {
		s.log.Warn("connect callback failed", "error", err.Error())
		writeHTML(w, http.StatusBadRequest, "Connection failed: "+err.Error())
		return
	}
	writeHTML(w, http.StatusOK, "Connected. You can close this window and return to Matrix.")
}

func (s *Server) postDisconnect(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.eng.Disconnect(r.Context(), owner, req.Provider); err != nil {
		writeJSON(w, http.StatusInternalServerError, types.Fail(types.NewError(types.CodeInternal, err.Error(), false)))
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]string{"status": "disconnected", "provider": req.Provider}))
}

// principal resolves the owner user id from the X-UWAC-Agent principal token.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	tok := strings.TrimSpace(r.Header.Get("X-UWAC-Agent"))
	if tok == "" {
		writeJSON(w, http.StatusUnauthorized, types.Fail(types.NewError(types.CodeUnauthorized, "missing X-UWAC-Agent principal token", false)))
		return "", false
	}
	owner, err := s.eng.OwnerFromToken(tok)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, types.Fail(types.NewError(types.CodeUnauthorized, "invalid principal token: "+err.Error(), false)))
		return "", false
	}
	return owner, true
}

// statusFor maps an error code to an HTTP status.
func statusFor(e *types.Error) int {
	if e == nil {
		return http.StatusOK
	}
	switch e.Code {
	case types.CodeInvalidRequest:
		return http.StatusBadRequest
	case types.CodeUnauthorized:
		return http.StatusUnauthorized
	case types.CodeNotConnected, types.CodeScopeMissing:
		return http.StatusForbidden
	case types.CodeNeedsConfirm:
		return http.StatusConflict
	case types.CodeProvider:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, types.Fail(types.NewError(types.CodeInvalidRequest, err.Error(), false)))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeHTML(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "<!doctype html><html><body style=\"font-family:system-ui;background:#0b0b0c;color:#e6e6e6;display:flex;align-items:center;justify-content:center;height:100vh;margin:0\"><p>"+msg+"</p></body></html>")
}
