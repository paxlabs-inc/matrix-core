// Package server wires the Deus HTTP control plane.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/discovery"
	"github.com/paxlabs-inc/deus/internal/gateway"
	"github.com/paxlabs-inc/deus/internal/hosting"
	"github.com/paxlabs-inc/deus/internal/registry"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/types"
)

const version = "0.1.0-phase3"

// Deps are long-lived services shared across handlers.
type Deps struct {
	Log               zerolog.Logger
	Store             *store.Store
	Chain             *chain.Client
	Registry          *registry.Service
	Discovery         *discovery.Service
	Gateway           *gateway.Gateway
	Hosting           *hosting.Orchestrator
	BlobURL           func(string) string
	DevMode           bool
	PublishPrivateKey string
}

// Server hosts HTTP routes.
type Server struct {
	deps Deps
	mux  chi.Router
}

// New constructs a Server with middleware and routes.
func New(deps Deps) *Server {
	s := &Server{deps: deps}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Get("/internal/healthz", s.handleHealthz)
	r.Handle("/internal/metrics", promStub())

	s.mountRegistryRoutes(r)
	s.mountDiscoveryRoutes(r)
	s.mountInvokeRoutes(r)

	s.mux = r
	return s
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := types.HealthResponse{
		OK:      true,
		Version: version,
	}
	if s.deps.Store != nil {
		resp.Postgres = s.deps.Store.Ping(ctx) == nil
		resp.OK = resp.OK && resp.Postgres
	}
	if s.deps.Chain != nil {
		resp.Chain = s.deps.Chain.Ping(ctx) == nil
	}
	status := http.StatusOK
	if !resp.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func promStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# deus metrics stub phase1\n"))
	})
}

// Shutdown is a hook for graceful drain (expanded in later phases).
func (s *Server) Shutdown(ctx context.Context) error {
	_ = ctx
	return nil
}
