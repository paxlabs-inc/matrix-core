package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/paxlabs-inc/tachyon-tools/internal/engine"
	"github.com/paxlabs-inc/tachyon-tools/pkg/rpc"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// Server exposes REST + JSON-RPC on one listener.
type Server struct {
	eng    *engine.Engine
	log    *slog.Logger
	mux    *http.ServeMux
	server *http.Server
	forge  string
	auth   string
}

// New constructs the HTTP API.
func New(eng *engine.Engine, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{eng: eng, log: log, mux: http.NewServeMux(), forge: probeForge(eng.Cfg.ForgePath), auth: eng.Cfg.AuthToken}
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /", s.handleRoot)
	s.mux.HandleFunc("POST /rpc", s.handleRPC)
	s.mux.HandleFunc("POST /v1/compile", s.postCompile)
	s.mux.HandleFunc("POST /v1/test", s.postTest)
	s.mux.HandleFunc("POST /v1/simulate", s.postSimulate)
	s.mux.HandleFunc("POST /v1/deploy", s.postDeploy)
	s.mux.HandleFunc("POST /v1/call", s.postCall)
	s.mux.HandleFunc("GET /v1/chains", s.getChains)
	s.mux.HandleFunc("POST /v1/chains", s.postChains)
	s.mux.HandleFunc("POST /v1/chains/use", s.postChainUse)
	s.mux.HandleFunc("GET /v1/artifacts/{name}", s.getArtifact)
	s.mux.HandleFunc("GET /v1/registry/deployments", s.getRegistry)
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
		s.log.Warn("tachyond: no auth_token set — all callers on this address can compile/deploy/sign; bind to loopback or set server.auth_token")
	}
	s.log.Info("tachyond listening", "addr", addr)
	return s.server.ListenAndServe()
}

// authMiddleware enforces a Bearer token on every request except GET /healthz
// and GET / when an auth token is configured. No-op when auth is disabled.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == "" || isPublicPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.auth)) != 1 {
			writeJSON(w, http.StatusUnauthorized, types.Fail[any](types.NewError(types.CodeInvalidRequest, "missing or invalid bearer token", false, nil)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicPath(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/")
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]string{
		"service": "tachyond",
		"version": engine.Version,
		"health":  "/healthz",
		"rpc":     "/rpc",
	}))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, types.OK(s.eng.Health(s.forge)))
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	rpc.ServeHTTP(w, r, s.eng)
}

func (s *Server) postCompile(w http.ResponseWriter, r *http.Request) {
	var req types.CompileRequest
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.eng.Compile(r.Context(), req))
}

func (s *Server) postTest(w http.ResponseWriter, r *http.Request) {
	var req types.TestRequest
	if !decode(w, r, &req) {
		return
	}
	env := s.eng.Test(r.Context(), req)
	status := http.StatusOK
	if !env.Ok {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, env)
}

func (s *Server) postSimulate(w http.ResponseWriter, r *http.Request) {
	var req types.SimulateRequest
	if !decode(w, r, &req) {
		return
	}
	env := s.eng.Simulate(r.Context(), req)
	status := http.StatusOK
	if !env.Ok {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, env)
}

func (s *Server) postDeploy(w http.ResponseWriter, r *http.Request) {
	var req types.DeployRequest
	if !decode(w, r, &req) {
		return
	}
	env := s.eng.Deploy(r.Context(), req)
	status := http.StatusOK
	if !env.Ok {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, env)
}

func (s *Server) postCall(w http.ResponseWriter, r *http.Request) {
	var req types.CallRequest
	if !decode(w, r, &req) {
		return
	}
	env := s.eng.Call(r.Context(), req)
	status := http.StatusOK
	if !env.Ok {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, env)
}

func (s *Server) getChains(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.ChainList())
}

func (s *Server) postChains(w http.ResponseWriter, r *http.Request) {
	var req types.ChainRegisterRequest
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.eng.ChainRegister(req))
}

func (s *Server) postChainUse(w http.ResponseWriter, r *http.Request) {
	var req types.ChainUseRequest
	if !decode(w, r, &req) {
		return
	}
	writeJSON(w, http.StatusOK, s.eng.ChainUse(req))
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	projectID := r.URL.Query().Get("project_id")
	writeJSON(w, http.StatusOK, s.eng.ArtifactGet(types.ArtifactGetRequest{
		Name:      name,
		ProjectID: projectID,
	}))
}

func (s *Server) getRegistry(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.RegistryLookup(types.RegistryLookupRequest{
		IdempotencyKey: r.URL.Query().Get("key"),
		ChainID:        r.URL.Query().Get("chain_id"),
	}))
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, types.Fail[any](types.NewError(types.CodeInvalidRequest, err.Error(), false, nil)))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func probeForge(forgePath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, forgePath, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
