package wallet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/internal/evm"
)

// EmbeddedSigner delegates signing to the Paxeer embedded wallet over the
// agent-native DID lane. The daemon's ed25519 seed proves a
// did:matrix:<label>:<keyfp> identity; the EVM key stays server-side and the
// wallet enforces custody policy (frozen / read_only / spend caps / allow-lists).
//
// Handshake mirrors tools/paxeer/lib/agentauth.mjs and the Go daemon identity:
//   POST /v1/agent/auth/challenge {did} -> {message, nonce}
//   ed25519-sign(message)
//   POST /v1/agent/auth/verify {did, public_key, nonce, signature} -> {token}
//   POST /v1/agent/sign {tx} (Bearer token) -> {signed_tx, address}
type EmbeddedSigner struct {
	baseURL string
	label   string
	priv    ed25519.PrivateKey
	pubHex  string
	did     string

	http  *http.Client
	token string
}

const defaultEmbeddedAPI = "https://connect.paxportwallet.com"

// NewEmbeddedSigner builds the embedded-wallet signer.
//
// Two operating modes, selected by whether a keyfile is configured:
//
//   - Single-tenant (keyfile set): loads the ed25519 seed, derives the agent
//     DID, and authenticates with it. Used when one tachyond instance serves
//     one agent (self-host / local dev).
//   - Multi-tenant token-only (keyfile empty): holds NO seed. Every signing
//     request must carry a forwarded TxIntent.AuthToken (the per-agent bearer
//     minted upstream). This is the shared-engine deployment: many agents,
//     zero seeds on the box.
func NewEmbeddedSigner(w config.WalletConfig) (*EmbeddedSigner, error) {
	label := w.Label
	if label == "" {
		label = "executor"
	}
	base := strings.TrimSuffix(strings.TrimSpace(w.API), "/")
	base = strings.TrimSuffix(base, "/v1")
	if base == "" {
		base = defaultEmbeddedAPI
	}
	s := &EmbeddedSigner{
		baseURL: base,
		label:   label,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
	if kf := strings.TrimSpace(w.Keyfile); kf != "" {
		raw, err := os.ReadFile(kf)
		if err != nil {
			return nil, fmt.Errorf("read embedded keyfile: %w", err)
		}
		seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("embedded keyfile must be a %d-byte (64-hex) ed25519 seed", ed25519.SeedSize)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pubHex := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
		s.priv = priv
		s.pubHex = pubHex
		s.did = fmt.Sprintf("did:matrix:%s:%s", label, pubHex[:16])
	}
	return s, nil
}

// DID returns the agent identity.
func (s *EmbeddedSigner) DID() string { return s.did }

// Address fetches the embedded wallet address (provisioning on first use).
func (s *EmbeddedSigner) Address(ctx context.Context) (common.Address, error) {
	var me struct {
		Wallet struct {
			Address string `json:"address"`
		} `json:"wallet"`
	}
	if err := s.agentCall(ctx, http.MethodGet, "/v1/agent/me", nil, &me); err != nil {
		return common.Address{}, err
	}
	if me.Wallet.Address == "" {
		if err := s.agentCall(ctx, http.MethodPost, "/v1/agent/provision", nil, &me); err != nil {
			return common.Address{}, err
		}
	}
	return common.HexToAddress(me.Wallet.Address), nil
}

// Sign delegates BOTH signing and broadcasting to the embedded wallet server
// (POST /v1/agent/send): the EVM key, nonce, and broadcast all live
// network-side, and custody policy (frozen / read_only / spend caps /
// allow-lists) is enforced there. The returned SignResult carries the
// resulting tx hash; the caller waits for the receipt over its own RPC.
//
// When intent.AuthToken is set it is used directly as the agent bearer (shared
// multi-tenant engine acting as that agent); otherwise the signer's own
// seed-derived token is used (single-tenant).
func (s *EmbeddedSigner) Sign(ctx context.Context, _ *evm.Client, intent TxIntent) (SignResult, error) {
	tx := map[string]any{}
	if strings.TrimSpace(intent.To) != "" {
		tx["to"] = intent.To
	}
	if len(intent.Data) > 0 {
		tx["data"] = "0x" + hex.EncodeToString(intent.Data)
	}
	value := "0"
	if intent.Value != nil {
		value = intent.Value.String()
	}
	tx["value"] = value
	if intent.Gas > 0 {
		tx["gas"] = intent.Gas
	}

	var out struct {
		TxHash  string `json:"tx_hash"`
		Address string `json:"address"`
	}
	if err := s.send(ctx, intent.AuthToken, map[string]any{"tx": tx}, &out); err != nil {
		return SignResult{}, err
	}
	if out.TxHash == "" {
		return SignResult{}, fmt.Errorf("embedded wallet returned no tx_hash")
	}
	return SignResult{TxHash: out.TxHash, From: common.HexToAddress(out.Address)}, nil
}

// send POSTs /v1/agent/send. A non-empty forwarded token is used verbatim
// (multi-tenant); otherwise the signer authenticates with its own seed.
func (s *EmbeddedSigner) send(ctx context.Context, token string, body, out any) error {
	if t := strings.TrimSpace(token); t != "" {
		return s.do(ctx, http.MethodPost, "/v1/agent/send", body, out, t)
	}
	return s.agentCall(ctx, http.MethodPost, "/v1/agent/send", body, out)
}

// authenticate runs the ed25519 challenge/verify handshake and caches the token.
func (s *EmbeddedSigner) authenticate(ctx context.Context) error {
	if s.priv == nil {
		return fmt.Errorf("embedded signer has no local seed; a forwarded wallet_token is required for this request")
	}
	var ch struct {
		Message string `json:"message"`
		Nonce   string `json:"nonce"`
	}
	if err := s.post(ctx, "/v1/agent/auth/challenge", map[string]any{"did": s.did}, &ch, ""); err != nil {
		return err
	}
	if ch.Message == "" || ch.Nonce == "" {
		return fmt.Errorf("agent auth: challenge returned no message/nonce")
	}
	sig := ed25519.Sign(s.priv, []byte(ch.Message))
	var vr struct {
		Token string `json:"token"`
	}
	body := map[string]any{
		"did":        s.did,
		"public_key": s.pubHex,
		"nonce":      ch.Nonce,
		"signature":  hex.EncodeToString(sig),
	}
	if err := s.post(ctx, "/v1/agent/auth/verify", body, &vr, ""); err != nil {
		return err
	}
	if vr.Token == "" {
		return fmt.Errorf("agent auth: verify returned no token")
	}
	s.token = vr.Token
	return nil
}

// agentCall performs an authed request, re-authenticating once on a 401.
func (s *EmbeddedSigner) agentCall(ctx context.Context, method, path string, body, out any) error {
	if s.token == "" {
		if err := s.authenticate(ctx); err != nil {
			return err
		}
	}
	err := s.do(ctx, method, path, body, out, s.token)
	if isUnauthorized(err) {
		if aerr := s.authenticate(ctx); aerr != nil {
			return aerr
		}
		return s.do(ctx, method, path, body, out, s.token)
	}
	return err
}

func (s *EmbeddedSigner) post(ctx context.Context, path string, body, out any, token string) error {
	return s.do(ctx, http.MethodPost, path, body, out, token)
}

type httpStatusError struct {
	code int
	body string
}

func (e *httpStatusError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("embedded wallet http %d: %s", e.code, e.body)
	}
	return fmt.Sprintf("embedded wallet http %d", e.code)
}

func isUnauthorized(err error) bool {
	se, ok := err.(*httpStatusError)
	return ok && se.code == http.StatusUnauthorized
}

func (s *EmbeddedSigner) do(ctx context.Context, method, path string, body, out any, token string) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpStatusError{code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
