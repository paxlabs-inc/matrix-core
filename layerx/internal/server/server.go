// Package server exposes layerxd over HTTP. Since PHASE 4 (full-transparency
// rollup model, layerx.frozen.kvx [api] overridden 2026-06-17) the surface is
// split into two groups:
//
//   - a PUBLIC, unauthenticated READ surface — the explorer/RPC: info, supply
//     (the reserve proof, invariant i1), batches, anchors, receipts, transfers,
//     accounts. Anyone can read it so receipts/roots are independently
//     verifiable (i4/i5) and the reserve is publicly auditable (i1).
//   - a WRITE/principal surface authorized by the DID signature ALONE (invariant
//     i6): pay/withdraw accept a directly DID-signed intent, OR an X-LayerX-Agent
//     principal token as a convenience. The shared transport bearer (LAYERX_TOKEN)
//     is now an OPTIONAL fleet gate (RequireTransport), never the public gate.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/events"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/ratelimit"
	"github.com/paxlabs-inc/layerx/internal/settle"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// ReserveReader reads the on-chain USDL reserve held in the vault — the
// on-chain side of the public reserve proof (invariant i1). *chain.Watcher
// satisfies it; it is nil when the chain is not configured (dev), in which case
// /v1/supply reports circulating supply only.
type ReserveReader interface {
	ReserveBalance(ctx context.Context) (*big.Int, error)
}

// Version is the layerxd build identity surfaced on /healthz and /.
const Version = "0.1.0"

const maxBodyBytes = 256 << 10

// swapOutRe constrains the optional withdrawal target asset symbol — it later
// keys an on-chain DEX swap, so it must be a clean uppercase ticker, not free
// text.
var swapOutRe = regexp.MustCompile(`^[A-Z0-9]{1,12}$`)

// evmAddrRe validates a 20-byte hex EVM payout address (0x + 40 hex chars).
var evmAddrRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// uuidRe validates a batch id so a malformed path returns 404, not a Postgres
// type-cast 500.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Server bundles the HTTP dependencies.
type Server struct {
	store            *store.Store
	ledger           *ledger.Ledger
	settler          *settle.Worker
	challenges       *auth.Challenges
	tokens           *auth.Tokens
	reserve          ReserveReader
	log              *slog.Logger
	transportToken   string
	requireTransport bool
	vaultAddr        string
	reserveAsset     string

	// public /v1/info descriptors
	chainID         int64
	anchorAddr      string
	usdlAddr        string
	dexRouter       string
	sequencerPubHex string
	window          time.Duration
	microThreshold  int64
	chainConfigured bool

	events *events.Broker // live SSE fan-out; nil = streaming disabled

	rl RateLimit
}

// RateLimit holds the application-level per-client limiters (defense in depth
// behind the nginx edge limiter). Each limiter is keyed by client IP. When
// Enabled is false (or a limiter is nil) that class is not throttled.
type RateLimit struct {
	Enabled bool
	// TrustProxy reads the client IP from X-Real-IP / X-Forwarded-For (set by
	// the nginx edge). Disable only when layerxd is directly internet-facing.
	TrustProxy bool
	Read       *ratelimit.Limiter // public reads / explorer
	Write      *ratelimit.Limiter // pay / withdraw / settle / account binding
	Auth       *ratelimit.Limiter // agent-auth challenge / verify
}

// Deps configures a Server.
type Deps struct {
	Store            *store.Store
	Ledger           *ledger.Ledger
	Settler          *settle.Worker
	Challenges       *auth.Challenges
	Tokens           *auth.Tokens
	Reserve          ReserveReader
	Log              *slog.Logger
	TransportToken   string
	RequireTransport bool
	VaultAddr        string
	ReserveAsset     string

	ChainID         int64
	AnchorAddr      string
	USDLAddr        string
	DEXRouter       string
	SequencerPubHex string
	Window          time.Duration
	MicroThreshold  int64
	ChainConfigured bool

	// Events is the live SSE broker for GET /v1/stream. Optional; nil disables
	// streaming (the endpoint then returns 503).
	Events *events.Broker

	RateLimit RateLimit
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
		store:            d.Store,
		ledger:           d.Ledger,
		settler:          d.Settler,
		challenges:       d.Challenges,
		tokens:           d.Tokens,
		reserve:          d.Reserve,
		log:              d.Log,
		transportToken:   d.TransportToken,
		requireTransport: d.RequireTransport,
		vaultAddr:        d.VaultAddr,
		reserveAsset:     d.ReserveAsset,
		chainID:          d.ChainID,
		anchorAddr:       d.AnchorAddr,
		usdlAddr:         d.USDLAddr,
		dexRouter:        d.DEXRouter,
		sequencerPubHex:  d.SequencerPubHex,
		window:           d.Window,
		microThreshold:   d.MicroThreshold,
		chainConfigured:  d.ChainConfigured,
		events:           d.Events,
		rl:               d.RateLimit,
	}
}

// Handler returns the fully-wired HTTP handler (optional transport auth wrapping
// the mux). The public READ surface and the auth lane are never transport-gated.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public read / explorer / RPC surface (no auth, PHASE 4).
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /v1/info", s.handleInfo)
	mux.HandleFunc("GET /v1/supply", s.handleSupply)
	mux.HandleFunc("GET /v1/batches", s.handleBatches)
	mux.HandleFunc("GET /v1/batch/{id}", s.handleBatch)
	mux.HandleFunc("GET /v1/anchor/{root}", s.handleAnchor)
	mux.HandleFunc("GET /v1/receipt/{seq}", s.handleReceipt)
	mux.HandleFunc("GET /v1/transfers", s.handleTransfers)
	mux.HandleFunc("GET /v1/account/{did}", s.handleAccount)
	mux.HandleFunc("GET /v1/stream", s.handleStream)

	// Auth lane (public; mints a principal token only on a valid DID signature).
	mux.HandleFunc("POST /v1/agent/auth/challenge", s.handleChallenge)
	mux.HandleFunc("POST /v1/agent/auth/verify", s.handleVerify)

	// Write / principal surface (DID-signed intent or principal token).
	mux.HandleFunc("GET /v1/balance", s.handleBalance)
	mux.HandleFunc("GET /v1/deposit", s.handleDeposit)
	mux.HandleFunc("POST /v1/account/evm", s.handleBindEVM)
	mux.HandleFunc("POST /v1/pay", s.handlePay)
	mux.HandleFunc("POST /v1/withdraw", s.handleWithdraw)
	mux.HandleFunc("POST /v1/settle", s.handleSettle)
	// Rate limit is the OUTERMOST layer (defense in depth behind nginx): it
	// throttles per client IP before auth so a flood can't even reach the
	// ledger or the token verifier.
	return s.rateLimitMiddleware(s.transportMiddleware(mux))
}

// rateLimitMiddleware applies the per-client token-bucket limiter for the
// request's class (read / write / auth). /healthz and / are exempt so liveness
// probes are never throttled. A denial returns 429 + Retry-After and the stable
// rate_limited code.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.rl.Enabled || r.URL.Path == "/" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		lim := s.limiterFor(r)
		if lim == nil {
			next.ServeHTTP(w, r)
			return
		}
		ok, retry := lim.Allow(s.clientIP(r))
		if !ok {
			secs := int(math.Ceil(retry.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeFail(w, http.StatusTooManyRequests, types.CodeRateLimited, "rate limit exceeded; slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limiterFor classifies a request into a limiter class. The agent-auth lane and
// the value-write POSTs get the tighter limiters; everything else is a read.
func (s *Server) limiterFor(r *http.Request) *ratelimit.Limiter {
	p := r.URL.Path
	if r.Method == http.MethodPost && (p == "/v1/agent/auth/challenge" || p == "/v1/agent/auth/verify") {
		return s.rl.Auth
	}
	if r.Method == http.MethodPost {
		return s.rl.Write
	}
	return s.rl.Read
}

// clientIP extracts the throttling key. Behind the nginx edge (TrustProxy) it
// honors X-Real-IP then the first X-Forwarded-For hop; otherwise it uses the
// transport peer address.
func (s *Server) clientIP(r *http.Request) string {
	if s.rl.TrustProxy {
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// transportMiddleware enforces the shared transport bearer on the
// write/principal endpoints ONLY when RequireTransport is set (legacy
// private-fleet mode). The public READ surface + auth lane are never gated, so
// the explorer/RPC works for anyone (PHASE 4, full-transparency model).
func (s *Server) transportMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.requireTransport || s.transportToken == "" || isPublicPath(r) {
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

// isPublicPath reports whether a request targets the always-open surface (the
// explorer/RPC reads + healthz/root + the auth lane), which the transport bearer
// never gates even in RequireTransport mode.
func isPublicPath(r *http.Request) bool {
	p := r.URL.Path
	if r.Method == http.MethodGet {
		switch p {
		case "/", "/healthz", "/v1/info", "/v1/supply", "/v1/batches", "/v1/transfers", "/v1/stream":
			return true
		}
		if strings.HasPrefix(p, "/v1/batch/") || strings.HasPrefix(p, "/v1/anchor/") ||
			strings.HasPrefix(p, "/v1/receipt/") || strings.HasPrefix(p, "/v1/account/") {
			return true
		}
	}
	if r.Method == http.MethodPost && (p == "/v1/agent/auth/challenge" || p == "/v1/agent/auth/verify") {
		return true
	}
	return false
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
	// Register the DID claim so the on-chain deposit watcher can reverse the
	// Vault Deposit `did` bytes32 (= keccak256(DID)) back to this account.
	claim := chain.DIDClaimHex(claims.DID)
	if err := s.store.RegisterDIDClaim(r.Context(), claim, claims.DID); err != nil {
		s.log.Error("register did claim failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not prepare deposit claim")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.DepositResponse{
		VaultAddress: s.vaultAddr,
		ReserveAsset: s.reserveAsset,
		DIDClaim:     "0x" + claim,
		Note:         "pass did_claim as the on-chain deposit `did` argument — the vault credits your balance ONLY through its deposit functions (depositUSDL/depositSwap/depositNative); a bare token transfer to the vault is NOT credited. Deposit USDL for 1:1 USDX, or USDC/USDT/PAX (atomically swapped to USDL at deposit; ERC-20 needs a prior approve). The depositing EVM address becomes your payout address.",
	}))
}

// handleBindEVM records/updates the caller DID's mapped Paxeer payout address
// (spec [usdx.account].binding). The address is scoped to the verified DID from
// the principal token, never a request field (invariant i6).
func (s *Server) handleBindEVM(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req types.BindEVMRequest
	if !decode(w, r, &req) {
		return
	}
	evm := strings.TrimSpace(req.EVMAddress)
	if !evmAddrRe.MatchString(evm) {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "evm_address must be a 0x-prefixed 20-byte hex address")
		return
	}
	if err := s.store.SetEVMAddress(r.Context(), claims.DID, evm); err != nil {
		s.log.Error("bind evm failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not bind payout address")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.BindEVMResponse{DID: claims.DID, EVMAddress: evm}))
}

func (s *Server) handlePay(w http.ResponseWriter, r *http.Request) {
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
	// Authorize by the X-LayerX-Agent token OR a DID-signed pay intent. The
	// canonical signed amount is the normalized 6dp decimal so the client and
	// server agree regardless of input formatting (invariant i6).
	preimage := auth.IntentMessage("pay", req.FromDID, req.Nonce, req.ToDID, types.FormatUSDX(amount))
	fromDID, ok := s.writeCaller(w, r, req.FromDID, req.PublicKey, req.Nonce, req.Signature, preimage)
	if !ok {
		return
	}
	receipt, err := s.ledger.Pay(r.Context(), fromDID, req.ToDID, amount)
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
	s.log.Info("transfer accepted", "seq", receipt.Seq, "from", fromDID, "to", req.ToDID, "tier", receipt.Tier)
	s.publishTransfer(receipt, fromDID, req.ToDID)
	writeJSON(w, http.StatusOK, types.OK(receipt))
}

// publishTransfer fans a newly-accepted transfer onto the live SSE stream so
// explorer clients see it without polling. Best-effort + nil-safe.
func (s *Server) publishTransfer(receipt types.Receipt, fromDID, toDID string) {
	if s.events == nil {
		return
	}
	data, err := json.Marshal(types.TransferView{
		Seq:         receipt.Seq,
		FromDID:     fromDID,
		ToDID:       toDID,
		AmountUSDX:  receipt.AmountUSDX,
		Tier:        receipt.Tier,
		LeafHashHex: receipt.LeafHashHex,
		Settled:     receipt.Settled,
		TS:          receipt.TS,
	})
	if err != nil {
		return
	}
	s.events.Publish(events.Event{Type: events.TypeTransfer, Seq: receipt.Seq, Data: data})
}

// handleReceipt is a PUBLIC read (PHASE 4): any signed receipt + inclusion proof
// + anchor is independently verifiable by anyone, with no DID-ownership scoping.
func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "seq must be an integer")
		return
	}
	receipt, err := s.ledger.ReceiptPublic(r.Context(), seq)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeFail(w, http.StatusNotFound, types.CodeNotFound, "receipt not found")
			return
		}
		s.log.Error("get receipt failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read receipt")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(receipt))
}

func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request) {
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
	// Authorize by the X-LayerX-Agent token OR a DID-signed withdraw intent over
	// the normalized 6dp amount + the canonical swap-out asset (invariant i6).
	preimage := auth.IntentMessage("withdraw", req.FromDID, req.Nonce, types.FormatUSDX(amount), swapOut)
	callerDID, ok := s.writeCaller(w, r, req.FromDID, req.PublicKey, req.Nonce, req.Signature, preimage)
	if !ok {
		return
	}
	// Withdrawals always force-settle (they move real reserve out of the vault).
	id, err := s.store.QueueWithdrawal(r.Context(), callerDID, amount, swapOut, types.TierMaterial)
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
	// Withdrawals move real reserve out of the vault, so they force-settle: kick
	// an async payout pass (idempotent + crash-safe) rather than wait for the window.
	if s.settler != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if _, err := s.settler.SettleWithdrawals(ctx); err != nil {
				s.log.Error("auto withdrawal payout failed", "withdrawal", id, "error", err.Error())
			}
		}()
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
	payoutRoot, err := s.settler.SettleWithdrawals(r.Context())
	if err != nil {
		s.log.Error("force withdrawal payout failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "withdrawal payout failed: "+err.Error())
		return
	}
	note := "nothing to settle"
	status := "noop"
	if batchID != "" || payoutRoot != "" {
		note = "settlement submitted"
		status = "anchored"
	}
	writeJSON(w, http.StatusOK, types.OK(types.SettleResponse{BatchID: batchID, Status: status, Note: note}))
}

// writeCaller authorizes a value write and returns the caller DID. It accepts
// EITHER the X-LayerX-Agent principal token (convenience) OR a DID-signed intent
// (the signature IS the authorization, invariant i6): from_did + public_key +
// nonce + signature over preimage. The nonce must be a live single-use challenge
// nonce bound to from_did, giving replay protection; it is consumed only after
// the signature verifies so a bad signature never burns a legitimate nonce.
func (s *Server) writeCaller(w http.ResponseWriter, r *http.Request, fromDID, pubHex, nonce, sigHex, preimage string) (string, bool) {
	if strings.TrimSpace(r.Header.Get("X-LayerX-Agent")) != "" {
		claims, ok := s.principal(w, r)
		if !ok {
			return "", false
		}
		return claims.DID, true
	}
	if fromDID == "" || sigHex == "" {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "authorize with an X-LayerX-Agent token or a DID-signed intent (from_did, public_key, nonce, signature)")
		return "", false
	}
	if _, err := auth.ParseDID(fromDID); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "from_did must be a valid did:matrix")
		return "", false
	}
	if err := auth.VerifyIntentSignature(fromDID, pubHex, sigHex, preimage); err != nil {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "intent signature: "+err.Error())
		return "", false
	}
	if !s.challenges.Consume(nonce, fromDID) {
		writeFail(w, http.StatusUnauthorized, types.CodeUnauthorized, "unknown, expired, or already-used nonce")
		return "", false
	}
	return fromDID, true
}

// ─── public read / explorer surface (PHASE 4) ───────────────────────────────

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, types.OK(types.InfoResponse{
		Service:            "layerxd",
		Version:            Version,
		ChainID:            s.chainID,
		VaultAddress:       s.vaultAddr,
		AnchorAddress:      s.anchorAddr,
		USDLAddress:        s.usdlAddr,
		DEXRouter:          s.dexRouter,
		ReserveAsset:       s.reserveAsset,
		SequencerPubkey:    s.sequencerPubHex,
		WindowSeconds:      int64(s.window.Seconds()),
		MicroThresholdUSDX: types.FormatUSDX(s.microThreshold),
		MicroPerUSDX:       types.MicroPerUSDX,
		ChainConfigured:    s.chainConfigured,
		TransportAuthGated: s.requireTransport && s.transportToken != "",
	}))
}

func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Supply(r.Context())
	if err != nil {
		s.log.Error("supply read failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read supply")
		return
	}
	resp := types.SupplyResponse{
		CirculatingUSDX: types.FormatUSDX(stats.CirculatingMicroUSDX),
		Accounts:        stats.Accounts,
		Transfers:       stats.Transfers,
	}
	// The on-chain reserve makes this the auditable reserve proof (invariant i1).
	// In dev / chain-unconfigured it is unavailable; report circulating only.
	if s.reserve != nil {
		reserve, rerr := s.reserve.ReserveBalance(r.Context())
		if rerr != nil {
			s.log.Warn("supply: reserve read failed", "error", rerr.Error())
		} else if reserve != nil {
			drift := new(big.Int).Sub(big.NewInt(stats.CirculatingMicroUSDX), reserve)
			resp.ReserveUSDL = formatBigMicro(reserve)
			resp.DriftUSDX = formatBigMicro(drift)
			resp.Reserved = drift.Sign() == 0
			resp.ReserveKnown = true
		}
	}
	writeJSON(w, http.StatusOK, types.OK(resp))
}

func (s *Server) handleBatches(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePage(r)
	rows, err := s.store.ListBatches(r.Context(), limit, offset)
	if err != nil {
		s.log.Error("list batches failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not list batches")
		return
	}
	views := make([]types.BatchView, 0, len(rows))
	for _, b := range rows {
		views = append(views, toBatchView(b))
	}
	writeJSON(w, http.StatusOK, types.OK(types.BatchesResponse{
		Batches: views, Limit: limit, Offset: offset, Count: len(views),
	}))
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !uuidRe.MatchString(id) {
		writeFail(w, http.StatusNotFound, types.CodeNotFound, "batch not found")
		return
	}
	b, err := s.store.GetBatch(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeFail(w, http.StatusNotFound, types.CodeNotFound, "batch not found")
			return
		}
		s.log.Error("get batch failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read batch")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(toBatchView(b)))
}

func (s *Server) handleAnchor(w http.ResponseWriter, r *http.Request) {
	root := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(r.PathValue("root")), "0x"))
	b, err := s.store.GetBatchByRoot(r.Context(), root)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeFail(w, http.StatusNotFound, types.CodeNotFound, "no batch for that root")
			return
		}
		s.log.Error("get anchor failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read anchor")
		return
	}
	writeJSON(w, http.StatusOK, types.OK(types.AnchorResponse{
		RootHex:      b.RootHex,
		BatchID:      b.ID,
		Status:       b.Status,
		AnchorTxHash: b.AnchorTx,
		Anchored:     b.Status == "anchored",
	}))
}

func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r)
	before := parseInt64(r.URL.Query().Get("before"))
	did := strings.TrimSpace(r.URL.Query().Get("did"))
	if did != "" {
		if _, err := auth.ParseDID(did); err != nil {
			writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "did filter must be a valid did:matrix")
			return
		}
	}
	rows, err := s.store.ListTransfers(r.Context(), did, limit, before)
	if err != nil {
		s.log.Error("list transfers failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not list transfers")
		return
	}
	views := make([]types.TransferView, 0, len(rows))
	for _, t := range rows {
		views = append(views, toTransferView(t))
	}
	resp := types.TransfersResponse{Transfers: views, DID: did, Limit: limit, Count: len(views)}
	// A full page implies more history; the smallest seq (DESC order = last row)
	// is the keyset cursor for the next older page (?before=).
	if len(views) == limit && limit > 0 {
		resp.NextBefore = views[len(views)-1].Seq
	}
	writeJSON(w, http.StatusOK, types.OK(resp))
}

// handleStream is the PUBLIC live event stream (SSE) backing the explorer — the
// scalable replacement for polling /v1/transfers. It emits `transfer` events
// (id = seq) as payments are accepted and `anchor` events as batches settle. On
// (re)connect it replays any transfers missed since the client's Last-Event-ID
// from the DB before joining the live tail, so no payment is ever silently
// dropped; a client that falls too far behind is told to resync + reconnect.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeFail(w, http.StatusServiceUnavailable, types.CodeInternal, "streaming not enabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // belt-and-suspenders: tell nginx not to buffer
	w.WriteHeader(http.StatusOK)

	// Replay cursor: Last-Event-ID (set by the browser on auto-reconnect) or an
	// explicit ?since= on first connect.
	cursor := parseInt64(r.Header.Get("Last-Event-ID"))
	if cursor <= 0 {
		cursor = parseInt64(r.URL.Query().Get("since"))
	}

	// Subscribe BEFORE replaying so live events during replay are buffered, not lost.
	ch, cancel := s.events.Subscribe()
	defer cancel()

	_, _ = io.WriteString(w, "retry: 3000\n\n")
	flusher.Flush()

	if cursor > 0 {
		if missed, mErr := s.store.ListTransfersSince(r.Context(), cursor, 500); mErr == nil {
			for _, t := range missed {
				if data, jErr := json.Marshal(toTransferView(t)); jErr == nil {
					writeSSE(w, t.Seq, events.TypeTransfer, data)
					cursor = t.Seq
				}
			}
			flusher.Flush()
		}
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				// Dropped for lagging: instruct the client to reconnect + replay.
				_, _ = io.WriteString(w, "event: resync\ndata: {\"reason\":\"lagged\"}\n\n")
				flusher.Flush()
				return
			}
			if ev.Type == events.TypeTransfer && ev.Seq <= cursor {
				continue // already replayed
			}
			writeSSE(w, ev.Seq, ev.Type, ev.Data)
			if ev.Type == events.TypeTransfer && ev.Seq > cursor {
				cursor = ev.Seq
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	did := strings.TrimSpace(r.PathValue("did"))
	if _, err := auth.ParseDID(did); err != nil {
		writeFail(w, http.StatusBadRequest, types.CodeInvalidRequest, "did must be a valid did:matrix")
		return
	}
	resp := types.AccountResponse{
		DID:         did,
		BalanceUSDX: types.FormatUSDX(0),
		EscrowUSDX:  types.FormatUSDX(0),
		History:     []types.TransferView{},
	}
	acct, err := s.store.GetAccount(r.Context(), did)
	switch {
	case err == nil:
		resp.EVMAddress = acct.EVMAddress
		resp.BalanceUSDX = types.FormatUSDX(acct.BalanceUSDX)
		resp.EscrowUSDX = types.FormatUSDX(acct.EscrowUSDX)
	case errors.Is(err, store.ErrNotFound):
		// Unknown account: zero balances + empty history (not a 404), so the
		// explorer can render any DID uniformly.
	default:
		s.log.Error("get account failed", "error", err.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read account")
		return
	}
	rows, terr := s.store.ListTransfers(r.Context(), did, parseLimit(r), 0)
	if terr != nil {
		s.log.Error("account history read failed", "error", terr.Error())
		writeFail(w, http.StatusInternalServerError, types.CodeInternal, "could not read account history")
		return
	}
	for _, t := range rows {
		resp.History = append(resp.History, toTransferView(t))
	}
	writeJSON(w, http.StatusOK, types.OK(resp))
}

// ─── helpers ──────────────────────────────────────────────────────────────

// parsePage reads ?limit= and ?offset= query params (the store clamps them to
// safe bounds). Used by the low-cardinality batch feed.
func parsePage(r *http.Request) (int, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	return limit, offset
}

// parseLimit reads ?limit=, clamped to [1,200] (default 50) to mirror the
// store's clamp so the handler can compute the keyset NextBefore cursor.
func parseLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 50
	}
	if n > 200 {
		n = 200
	}
	return n
}

// parseInt64 parses a base-10 int64 query/header value, 0 on absence/parse error.
func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// writeSSE writes one Server-Sent Event frame. A non-positive id omits the `id:`
// line (non-transfer events do not advance the Last-Event-ID cursor).
func writeSSE(w io.Writer, id int64, eventType string, data []byte) {
	if id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", id)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
}

// formatBigMicro renders a micro-precision big.Int as a 6dp USDX decimal when it
// fits in int64, else its raw integer string (defensive; reserves never realistically
// exceed int64 micro-USDX).
func formatBigMicro(v *big.Int) string {
	if v != nil && v.IsInt64() {
		return types.FormatUSDX(v.Int64())
	}
	return v.String()
}

func toBatchView(b store.BatchSummary) types.BatchView {
	return types.BatchView{
		ID:            b.ID,
		RootHex:       b.RootHex,
		Status:        b.Status,
		AnchorTxHash:  b.AnchorTx,
		TransferCount: b.TransferCount,
		WindowStart:   b.WindowStart,
		WindowEnd:     b.WindowEnd,
		CreatedAt:     b.CreatedAt,
	}
}

func toTransferView(t store.TransferSummary) types.TransferView {
	return types.TransferView{
		Seq:          t.Seq,
		BatchID:      t.BatchID,
		FromDID:      t.FromDID,
		ToDID:        t.ToDID,
		AmountUSDX:   types.FormatUSDX(t.AmountMicro),
		Tier:         t.Tier,
		LeafHashHex:  t.LeafHex,
		BatchRootHex: t.BatchRootHex,
		AnchorTxHash: t.AnchorTx,
		Settled:      t.Settled,
		TS:           t.TS,
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
