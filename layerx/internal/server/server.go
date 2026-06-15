// Package server exposes layerxd over HTTP: the agent-DID auth lane and the
// value surface (balance/deposit/pay/receipt/withdraw/settle). Two-layer auth
// (layerx.frozen.kvx [auth]): a shared transport bearer (LAYERX_TOKEN) proves "a
// legitimate Matrix daemon", and an ed25519 agent-DID principal token
// (X-LayerX-Agent) proves WHICH account — so balances/receipts scope on the
// verified DID, never a request field (invariant i6).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/settle"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// Version is the layerxd build identity surfaced on /healthz and /.
const Version = "0.1.0"

const maxBodyBytes = 256 << 10

// swapOutRe constrains the optional withdrawal target asset symbol — it later
// keys an on-chain DEX swap, so it must be a clean uppercase ticker, not free
// text.
var swapOutRe = regexp.MustCompile(`^[A-Z0-9]{1,12}$`)

// Server bundles the HTTP dependencies.
type Server struct {
	store          *store.Store
	ledger         *ledger.Ledger
	settler        *settle.Worker
	challenges     *auth.Challenges
	tokens         *auth.Tokens
	log            *slog.Logger
	transportToken string
	vaultAddr      string
	reserveAsset   string
}

// Deps configures a Server.
type Deps struct {
	Store          *store.Store
	Ledger         *ledger.Ledger
	Settler        *settle.Worker
	Challenges     *auth.Challenges
	Tokens         *auth.Tokens
	Log            *slog.Logger
	TransportToken string
	VaultAddr      string
	ReserveAsset   string
}

// New builds the Server.
func New(d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ReserveAsset == "" {
		d.ReserveAsset = "USDL"
	}
	return &Server{
		store:          d.Store,
		ledger:         d.Ledger,
		settler:        d.Settler,
		challenges:     d.Challenges,
		tokens:         d.Tokens,
		log:            d.Log,
		transportToken: d.TransportToken,
		vaultAddr:      d.VaultAddr,
		reserveAsset:   d.ReserveAsset,
	}
}

// Handler returns the fully-wired HTTP handler (transport auth wrapping the mux).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("POST /v1/agent/auth/challenge", s.handleChallenge)
	mux.HandleFunc("POST /v1/agent/auth/verify", s.handleVerify)
	mux.HandleFunc("GET /v1/balance", s.handleBalance)
	mux.HandleFunc("GET /v1/deposit", s.handleDeposit)
	mux.HandleFunc("POST /v1/pay", s.handlePay)
	mux.HandleFunc("GET /v1/receipt/{seq}", s.handleReceipt)
	mux.HandleFunc("POST /v1/withdraw", s.handleWithdraw)
	mux.HandleFunc("POST /v1/settle", s.handleSettle)
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
		"service": "layerxd",
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
	token, expiresIn := s.tokens.Mint(req.DID)
	writeJSON(w, http.StatusOK, types.OK(types.VerifyResponse{
		Token:     token,
		DID:       req.DID,
		ExpiresIn: expiresIn,
	}))
}

// principal resolves the verified DID from the X-LayerX-Agent token.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	tok := strings.TrimSpace(r.Header.Get("X-LayerX-Agent"))
	if tok == "" {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "missing X-LayerX-Agent principal token")
		return auth.Claims{}, false
	}
	claims, err := s.tokens.Verify(tok)
	if err != nil {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "invalid principal token: "+err.Error())
		return auth.Claims{}, false
	}
	return claims, true
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	acct, err := s.store.GetAccount(r.Context(), claims.DID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unfunded account: report a zero balance rather than 404.
			writeJSON(w, http.StatusOK, types.OK(types.BalanceResponse{
				DID:         claims.DID,
				BalanceUSDX: types.FormatUSDX(0),
				EscrowUSDX:  types.FormatUSDX(0),
			}))
			return
		}
		s.log.Error("get balance failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read balance")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.BalanceResponse{
		DID:         acct.DID,
		EVMAddress:  acct.EVMAddress,
		BalanceUSDX: types.FormatUSDX(acct.BalanceUSDX),
		EscrowUSDX:  types.FormatUSDX(acct.EscrowUSDX),
	}))
}

func (s *Server) handleDeposit(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.DepositResponse{
		VaultAddress: s.vaultAddr,
		ReserveAsset: s.reserveAsset,
		DIDClaim:     "layerx-deposit-claim:" + claims.DID,
		Note:         "deposit USDL for 1:1 USDX, or USDC/USDT/PAX (atomically swapped to USDL at deposit). Funding from the user wallet escalates to MCL.",
	}))
}

func (s *Server) handlePay(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req types.PayRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := auth.ParseDID(req.ToDID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "to_did must be a valid did:matrix")
		return
	}
	amount, err := types.ParseUSDX(req.AmountUSDX)
	if err != nil || amount <= 0 {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "amount_usdx must be a positive USDX decimal")
		return
	}
	receipt, err := s.ledger.Pay(r.Context(), claims.DID, req.ToDID, amount)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInsufficientFunds):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "insufficient escrow-backed balance")
		case errors.Is(err, store.ErrNotFound):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "account not funded; deposit first")
		default:
			s.log.Error("pay failed", "error", err.Error())
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not complete payment")
		}
		return
	}
	// Material transfers auto-promote to force-settle (layerx.frozen.kvx
	// [settlement.tiers]); micropayments wait for the window.
	if receipt.Tier == types.TierMaterial && s.settler != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := s.settler.SettleNow(ctx); err != nil {
				s.log.Error("auto force-settle failed", "seq", receipt.Seq, "error", err.Error())
			}
		}()
	}
	s.log.Info("transfer accepted", "seq", receipt.Seq, "from", claims.DID, "to", req.ToDID, "tier", receipt.Tier)
	writeJSON(w, http.StatusOK, types.OK(receipt))
}

func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "seq must be an integer")
		return
	}
	receipt, err := s.ledger.Receipt(r.Context(), seq, claims.DID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeFail(w, http.StatusNotFound, types.CodeNotFound, "receipt not found or not owned")
			return
		}
		s.log.Error("get receipt failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read receipt")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(receipt))
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req types.WithdrawRequest
	if !decode(w, r, &req) {
		return
	}
	amount, err := types.ParseUSDX(req.AmountUSDX)
	if err != nil || amount <= 0 {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "amount_usdx must be a positive USDX decimal")
		return
	}
	swapOut := strings.ToUpper(strings.TrimSpace(req.SwapOut))
	if swapOut != "" && !swapOutRe.MatchString(swapOut) {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "swap_out must be an asset ticker [A-Z0-9]{1,12} (or empty for USDL)")
		return
	}
	// Withdrawals always force-settle (they move real reserve out of the vault).
	id, err := s.store.QueueWithdrawal(r.Context(), claims.DID, amount, swapOut, types.TierMaterial)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInsufficientFunds):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "insufficient escrow-backed balance")
		case errors.Is(err, store.ErrNotFound):
			writeFail(w, http.StatusPaymentRequired, types.CodeInsufficientFunds, "account not funded")
		default:
			s.log.Error("withdraw failed", "error", err.Error())
			writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not queue withdrawal")
		}
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.WithdrawResponse{
		WithdrawalID: id,
		AmountUSDX:   types.FormatUSDX(amount),
		Tier:         types.TierMaterial,
		Status:       "queued",
	}))
}

func (s *Server) handleSettle(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r); !ok {
		return
	}
	if s.settler == nil {
		writeFail(w, http.StatusServiceUnavailable, types.CodeInternal, "settlement worker not configured")
		return
	}
	batchID, err := s.settler.SettleNow(r.Context())
	if err != nil {
		s.log.Error("force settle failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "settlement failed: "+err.Error())
		return
	}
	note := "nothing to settle"
	status := "noop"
	if batchID != "" {
		note = "batch sealed + anchored"
		status = "anchored"
	}
	writeJSON(w, http.StatusOK, types.OK(types.SettleResponse{BatchID: batchID, Status: status, Note: note}))
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
