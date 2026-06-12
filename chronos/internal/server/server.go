// Package server exposes chronosd over HTTP: the agent-DID auth lane and the
// alarm CRUD surface. Two-layer auth (chronos.frozen.kvx [auth]): a shared
// transport bearer (CHRONOS_TOKEN) proves "a legitimate Matrix daemon", and an
// ed25519 agent-DID principal token (X-Chronos-Agent) proves WHICH owner — so
// alarms are owner-scoped and the wake target resolves from the DID alone
// (invariant i2).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/paxlabs-inc/chronos/internal/auth"
	"github.com/paxlabs-inc/chronos/internal/schedule"
	"github.com/paxlabs-inc/chronos/internal/store"
	"github.com/paxlabs-inc/chronos/pkg/types"
)

// Version is the chronosd build identity surfaced on /healthz and /.
const Version = "0.1.0"

const maxBodyBytes = 256 << 10

// Server bundles the HTTP dependencies.
type Server struct {
	store          *store.Store
	challenges     *auth.Challenges
	tokens         *auth.Tokens
	log            *slog.Logger
	transportToken string
	defaultMaxFail int
}

// Deps configures a Server.
type Deps struct {
	Store          *store.Store
	Challenges     *auth.Challenges
	Tokens         *auth.Tokens
	Log            *slog.Logger
	TransportToken string
	DefaultMaxFail int
}

// New builds the Server.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.DefaultMaxFail <= 0 {
		d.DefaultMaxFail = 5
	}
	return &Server{
		store:          d.Store,
		challenges:     d.Challenges,
		tokens:         d.Tokens,
		log:            d.Log,
		transportToken: d.TransportToken,
		defaultMaxFail: d.DefaultMaxFail,
	}
}

// Handler returns the fully-wired HTTP handler (transport auth wrapping the mux).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("POST /v1/agent/auth/challenge", s.handleChallenge)
	mux.HandleFunc("POST /v1/agent/auth/verify", s.handleVerify)
	mux.HandleFunc("POST /v1/alarms", s.handleCreateAlarm)
	mux.HandleFunc("GET /v1/alarms", s.handleListAlarms)
	mux.HandleFunc("GET /v1/alarms/{id}", s.handleGetAlarm)
	mux.HandleFunc("DELETE /v1/alarms/{id}", s.handleCancelAlarm)
	return s.transportMiddleware(mux)
}

// transportMiddleware enforces the shared transport bearer on every path except
// the public healthz/root.
func (s *Server) transportMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.transportToken == "" || isPublicPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.transportToken)) != 1 {
			writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "missing or invalid transport bearer")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicPath(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/healthz")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]string{
		"service": "chronosd",
		"version": Version,
		"health":  "/healthz",
	}))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	dbOK := s.store.Ping(ctx) == nil
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, types.OK(map[string]any{"status": "ok", "version": Version, "db": dbOK}))
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	var req types.ChallengeRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := auth.ParseDID(req.DID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, err.Error())
		return
	}
	nonce, msg := s.challenges.Create(req.DID)
	writeJSON(w, http.StatusOK, types.OK(types.ChallengeResponse{
		DID:       req.DID,
		Nonce:     nonce,
		Message:   msg,
		ExpiresIn: int(s.challenges.TTL().Seconds()),
	}))
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req types.VerifyRequest
	if !decode(w, r, &req) {
		return
	}
	if !s.challenges.Consume(req.Nonce, req.DID) {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "unknown, expired, or already-used nonce")
		return
	}
	if err := auth.VerifySignature(req.DID, req.PublicKey, req.Nonce, req.Signature); err != nil {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, err.Error())
		return
	}
	did, err := auth.ParseDID(req.DID)
	if err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, err.Error())
		return
	}
	owner := auth.OwnerFromDID(did)
	token, expiresIn := s.tokens.Mint(req.DID, owner)
	writeJSON(w, http.StatusOK, types.OK(types.VerifyResponse{
		Token:       token,
		OwnerUserID: owner,
		ExpiresIn:   expiresIn,
	}))
}

// principal resolves the verified {did, owner} from the X-Chronos-Agent token.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	tok := strings.TrimSpace(r.Header.Get("X-Chronos-Agent"))
	if tok == "" {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "missing X-Chronos-Agent principal token")
		return auth.Claims{}, false
	}
	claims, err := s.tokens.Verify(tok)
	if err != nil {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "invalid principal token: "+err.Error())
		return auth.Claims{}, false
	}
	return claims, true
}

func (s *Server) handleCreateAlarm(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req types.CreateAlarmRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.WakeMessage) == "" {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "wake_message is required (the contextful turn delivered on wake)")
		return
	}

	now := time.Now().UTC()
	alarm := types.Alarm{
		OwnerDID:       claims.DID,
		UserID:         claims.Owner,
		Label:          strings.TrimSpace(req.Label),
		ConversationID: strings.TrimSpace(req.ConversationID),
		WakeMessage:    req.WakeMessage,
		Payload:        req.Payload,
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		MaxFailures:    req.MaxFailures,
	}
	if alarm.MaxFailures <= 0 {
		alarm.MaxFailures = s.defaultMaxFail
	}

	switch req.Kind {
	case types.KindOnce:
		next, err := schedule.NextOnce(req.DelaySeconds, req.FireAt, now)
		if err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, err.Error())
			return
		}
		alarm.Kind = types.KindOnce
		alarm.FireAt = &next
		alarm.NextFireAt = next
		alarm.Timezone = "UTC"
	case types.KindCron:
		tz := req.Timezone
		if tz == "" {
			tz = "UTC"
		}
		next, err := schedule.NextCron(req.CronExpr, tz, now)
		if err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, err.Error())
			return
		}
		alarm.Kind = types.KindCron
		alarm.CronExpr = req.CronExpr
		alarm.Timezone = tz
		alarm.NextFireAt = next
	default:
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "kind must be 'once' or 'cron'")
		return
	}

	created, deduped, err := s.store.CreateAlarm(r.Context(), alarm)
	if err != nil {
		s.log.Error("create alarm failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not create alarm")
		return
	}
	if deduped {
		s.log.Info("alarm idempotent hit", "alarm", created.ID, "owner", created.OwnerDID)
	} else {
		s.log.Info("alarm created", "alarm", created.ID, "kind", created.Kind, "user", created.UserID, "next_fire_at", created.NextFireAt.Format(time.RFC3339))
	}
	writeJSON(w, http.StatusOK, types.OK(types.CreateAlarmResponse{
		ID:         created.ID,
		NextFireAt: created.NextFireAt,
		Status:     created.Status,
	}))
}

func (s *Server) handleListAlarms(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	alarms, err := s.store.ListAlarms(r.Context(), claims.DID, limit)
	if err != nil {
		s.log.Error("list alarms failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not list alarms")
		return
	}
	views := make([]types.View, 0, len(alarms))
	for _, a := range alarms {
		views = append(views, types.ViewOf(a))
	}
	writeJSON(w, http.StatusOK, types.OK(map[string]any{"alarms": views, "count": len(views)}))
}

func (s *Server) handleGetAlarm(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	a, err := s.store.GetAlarm(r.Context(), r.PathValue("id"), claims.DID)
	if err != nil {
		s.notFoundOrInternal(w, err, "get alarm")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.ViewOf(a)))
}

func (s *Server) handleCancelAlarm(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	a, err := s.store.CancelAlarm(r.Context(), r.PathValue("id"), claims.DID)
	if err != nil {
		s.notFoundOrInternal(w, err, "cancel alarm")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.ViewOf(a)))
}

func (s *Server) notFoundOrInternal(w http.ResponseWriter, err error, op string) {
	if err == store.ErrNotFound {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "alarm not found")
		return
	}
	s.log.Error(op+" failed", "error", err.Error())
	writeFail(w, http.StatusInternalServerError, types.CodeInternal, "internal error")
}

// ─── helpers ──────────────────────────────────────────────────────────────

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(v); err != nil && err != io.EOF {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "invalid json body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeFail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, types.Fail(types.NewError(code, msg, false)))
}
