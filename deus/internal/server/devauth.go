package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/go-chi/chi/v5"
)

// Developer authentication (docs/05-api.md §5.1, implemented).
//
// Owner-scoped routes used to trust a bare X-Developer-Wallet header, which
// let anyone act as any developer (including redirecting payouts). The
// production path is now Sign-In-With-Ethereum (EIP-4361):
//
//	POST /v1/developers/nonce  -> stateless HMAC nonce {nonce, expires_at}
//	POST /v1/developers/auth   -> {message, signature} verified via EIP-191
//	                              personal_sign recovery; returns a short-lived
//	                              HMAC token bound to the recovered wallet
//
// The token travels as X-Developer-Token. Bare wallet headers are accepted
// only when DEUS_DEV=1.
//
// Nonces are stateless (random || expiry, HMAC-signed) and therefore
// replayable within their 5-minute window — acceptable because replaying a
// {message, signature} pair only re-issues a token for the same wallet the
// signature already proves control of.

const (
	devAuthNonceTTL     = 5 * time.Minute
	devAuthTokenTTL     = 24 * time.Hour
	maxSIWEMessageBytes = 4096
	maxDevAuthBodyBytes = 16 * 1024
)

// DeveloperAuth verifies SIWE sign-ins and mints/verifies developer tokens.
type DeveloperAuth struct {
	secret     []byte
	siweDomain string
	now        func() time.Time
}

// NewDeveloperAuth returns nil when secret is empty (feature unavailable;
// dev-mode header fallback remains the only path).
func NewDeveloperAuth(secret, siweDomain string) *DeveloperAuth {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil
	}
	return &DeveloperAuth{
		secret:     []byte(secret),
		siweDomain: strings.TrimSpace(siweDomain),
		now:        time.Now,
	}
}

func (a *DeveloperAuth) sign(payload string) string {
	mac := hmac.New(sha256.New, a.secret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *DeveloperAuth) verifySigned(kind, raw, sig string) bool {
	expected := a.sign(kind + ":" + raw)
	return hmac.Equal([]byte(expected), []byte(sig))
}

// IssueNonce mints a stateless, HMAC-bound nonce for the SIWE message.
func (a *DeveloperAuth) IssueNonce() (nonce string, expiresAt time.Time, err error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	expiresAt = a.now().Add(devAuthNonceTTL)
	raw := hex.EncodeToString(buf) + "." + strconv.FormatInt(expiresAt.Unix(), 10)
	nonce = base64.RawURLEncoding.EncodeToString([]byte(raw)) + "." + a.sign("nonce:"+raw)
	return nonce, expiresAt, nil
}

func (a *DeveloperAuth) verifyNonce(nonce string) error {
	encoded, sig, ok := strings.Cut(nonce, ".")
	if !ok {
		return errors.New("malformed nonce")
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("malformed nonce")
	}
	raw := string(rawBytes)
	if !a.verifySigned("nonce", raw, sig) {
		return errors.New("nonce signature mismatch")
	}
	_, expStr, ok := strings.Cut(raw, ".")
	if !ok {
		return errors.New("malformed nonce payload")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return errors.New("malformed nonce expiry")
	}
	if a.now().Unix() > exp {
		return errors.New("nonce expired")
	}
	return nil
}

// MintToken issues a short-lived developer token bound to wallet.
func (a *DeveloperAuth) MintToken(wallet string) (token string, expiresAt time.Time) {
	expiresAt = a.now().Add(devAuthTokenTTL)
	raw := strings.ToLower(strings.TrimSpace(wallet)) + "." + strconv.FormatInt(expiresAt.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw)) + "." + a.sign("token:"+raw), expiresAt
}

// VerifyToken returns the wallet a valid, unexpired token is bound to.
func (a *DeveloperAuth) VerifyToken(token string) (string, error) {
	encoded, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return "", errors.New("malformed token")
	}
	rawBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("malformed token")
	}
	raw := string(rawBytes)
	if !a.verifySigned("token", raw, sig) {
		return "", errors.New("token signature mismatch")
	}
	wallet, expStr, ok := strings.Cut(raw, ".")
	if !ok {
		return "", errors.New("malformed token payload")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", errors.New("malformed token expiry")
	}
	if a.now().Unix() > exp {
		return "", errors.New("token expired")
	}
	if !evmAddressRe.MatchString(wallet) {
		return "", errors.New("malformed token wallet")
	}
	return wallet, nil
}

// ─── SIWE (EIP-4361) ────────────────────────────────────────────────────────

var (
	siwePreambleRe = regexp.MustCompile(`^(\S+) wants you to sign in with your Ethereum account:$`)
	evmAddressRe   = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

type siweFields struct {
	domain         string
	address        string
	nonce          string
	expirationTime string
}

func parseSIWE(message string) (siweFields, error) {
	var f siweFields
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return f, errors.New("message too short")
	}
	m := siwePreambleRe.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if m == nil {
		return f, errors.New("missing sign-in preamble")
	}
	f.domain = m[1]
	addr := strings.TrimSpace(lines[1])
	if !evmAddressRe.MatchString(addr) {
		return f, errors.New("missing account address")
	}
	f.address = addr
	for _, ln := range lines[2:] {
		ln = strings.TrimSpace(ln)
		if v, ok := strings.CutPrefix(ln, "Nonce: "); ok {
			f.nonce = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(ln, "Expiration Time: "); ok {
			f.expirationTime = strings.TrimSpace(v)
		}
	}
	if f.nonce == "" {
		return f, errors.New("missing nonce")
	}
	return f, nil
}

// personalSignDigest hashes message per EIP-191 (personal_sign).
func personalSignDigest(message string) []byte {
	prefixed := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	return crypto.Keccak256([]byte(prefixed))
}

// Authenticate verifies a SIWE message + signature and returns the wallet.
func (a *DeveloperAuth) Authenticate(message, signature string) (string, error) {
	if len(message) == 0 || len(message) > maxSIWEMessageBytes {
		return "", errors.New("message size out of bounds")
	}
	fields, err := parseSIWE(message)
	if err != nil {
		return "", fmt.Errorf("invalid siwe message: %w", err)
	}
	if a.siweDomain != "" && !strings.EqualFold(fields.domain, a.siweDomain) {
		return "", fmt.Errorf("domain %q not allowed", fields.domain)
	}
	if err := a.verifyNonce(fields.nonce); err != nil {
		return "", fmt.Errorf("invalid nonce: %w", err)
	}
	if fields.expirationTime != "" {
		exp, err := time.Parse(time.RFC3339, fields.expirationTime)
		if err != nil {
			return "", errors.New("invalid expiration time")
		}
		if a.now().After(exp) {
			return "", errors.New("message expired")
		}
	}

	sigBytes, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(signature), "0x"))
	if err != nil || len(sigBytes) != 65 {
		return "", errors.New("malformed signature")
	}
	sig := make([]byte, 65)
	copy(sig, sigBytes)
	if sig[64] >= 27 {
		sig[64] -= 27 // MetaMask-style v ∈ {27,28} -> {0,1}
	}
	pub, err := crypto.SigToPub(personalSignDigest(message), sig)
	if err != nil {
		return "", errors.New("signature recovery failed")
	}
	recovered := crypto.PubkeyToAddress(*pub)
	if !strings.EqualFold(recovered.Hex(), fields.address) {
		return "", errors.New("signature does not match account")
	}
	return strings.ToLower(recovered.Hex()), nil
}

// ─── HTTP surface ───────────────────────────────────────────────────────────

type developerNonceResponse struct {
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expires_at"`
}

type developerAuthRequest struct {
	Message   string `json:"message"`
	Signature string `json:"signature"`
}

type developerAuthResponse struct {
	Wallet    string    `json:"wallet"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) mountDeveloperAuthRoutes(r chi.Router) {
	r.Route("/v1/developers", func(r chi.Router) {
		r.Post("/nonce", s.handleDeveloperNonce)
		r.Post("/auth", s.handleDeveloperAuth)
	})
}

func (s *Server) handleDeveloperNonce(w http.ResponseWriter, _ *http.Request) {
	if s.devAuth == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "developer auth not configured", nil)
		return
	}
	nonce, exp, err := s.devAuth.IssueNonce()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "nonce generation failed", nil)
		return
	}
	writeJSON(w, http.StatusOK, developerNonceResponse{Nonce: nonce, ExpiresAt: exp})
}

func (s *Server) handleDeveloperAuth(w http.ResponseWriter, r *http.Request) {
	if s.devAuth == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable", "developer auth not configured", nil)
		return
	}
	var body developerAuthRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxDevAuthBodyBytes)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid json body", nil)
		return
	}
	wallet, err := s.devAuth.Authenticate(body.Message, body.Signature)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", err.Error(), nil)
		return
	}
	token, exp := s.devAuth.MintToken(wallet)
	writeJSON(w, http.StatusOK, developerAuthResponse{Wallet: wallet, Token: token, ExpiresAt: exp})
}
