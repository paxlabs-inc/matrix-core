package server

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

func testDevAuth(t *testing.T) *DeveloperAuth {
	t.Helper()
	a := NewDeveloperAuth("unit-test-secret", "")
	if a == nil {
		t.Fatal("NewDeveloperAuth returned nil for non-empty secret")
	}
	return a
}

func siweMessage(domain, address, nonce, expiration string) string {
	lines := []string{
		domain + " wants you to sign in with your Ethereum account:",
		address,
		"",
		"Link your wallet to the Deus marketplace.",
		"",
		"URI: https://" + domain,
		"Version: 1",
		"Chain ID: 125",
		"Nonce: " + nonce,
		"Issued At: " + time.Now().UTC().Format(time.RFC3339),
	}
	if expiration != "" {
		lines = append(lines, "Expiration Time: "+expiration)
	}
	return strings.Join(lines, "\n")
}

func signSIWE(t *testing.T, key *ecdsa.PrivateKey, message string) string {
	t.Helper()
	sig, err := crypto.Sign(personalSignDigest(message), key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Wallets return v in {27,28}; exercise that form.
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig)
}

func TestDeveloperAuthRoundTrip(t *testing.T) {
	a := testDevAuth(t)
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()

	nonce, _, err := a.IssueNonce()
	if err != nil {
		t.Fatalf("issue nonce: %v", err)
	}
	msg := siweMessage("market.example", addr, nonce, "")
	wallet, err := a.Authenticate(msg, signSIWE(t, key, msg))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if !strings.EqualFold(wallet, addr) {
		t.Fatalf("wallet mismatch: got %s want %s", wallet, addr)
	}

	token, _ := a.MintToken(wallet)
	got, err := a.VerifyToken(token)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if got != strings.ToLower(addr) {
		t.Fatalf("token wallet mismatch: got %s", got)
	}
}

func TestDeveloperAuthRejectsTamperedSignature(t *testing.T) {
	a := testDevAuth(t)
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()

	nonce, _, _ := a.IssueNonce()
	msg := siweMessage("market.example", addr, nonce, "")
	sig := signSIWE(t, key, msg)

	// Flip a nibble in the middle of the signature.
	tampered := []byte(sig)
	if tampered[20] == 'a' {
		tampered[20] = 'b'
	} else {
		tampered[20] = 'a'
	}
	if _, err := a.Authenticate(msg, string(tampered)); err == nil {
		t.Fatal("expected tampered signature to be rejected")
	}

	// Signature from a DIFFERENT key over the same message.
	otherKey, _ := crypto.GenerateKey()
	if _, err := a.Authenticate(msg, signSIWE(t, otherKey, msg)); err == nil {
		t.Fatal("expected wrong-signer signature to be rejected")
	}
}

func TestDeveloperAuthRejectsForeignAndExpiredNonce(t *testing.T) {
	a := testDevAuth(t)
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()

	// Nonce minted under a different secret.
	other := NewDeveloperAuth("some-other-secret", "")
	foreignNonce, _, _ := other.IssueNonce()
	msg := siweMessage("market.example", addr, foreignNonce, "")
	if _, err := a.Authenticate(msg, signSIWE(t, key, msg)); err == nil {
		t.Fatal("expected foreign nonce to be rejected")
	}

	// Expired nonce: issue, then advance the clock past the TTL.
	nonce, _, _ := a.IssueNonce()
	a.now = func() time.Time { return time.Now().Add(devAuthNonceTTL + time.Minute) }
	msg = siweMessage("market.example", addr, nonce, "")
	if _, err := a.Authenticate(msg, signSIWE(t, key, msg)); err == nil {
		t.Fatal("expected expired nonce to be rejected")
	}
}

func TestDeveloperAuthRejectsExpiredMessageAndToken(t *testing.T) {
	a := testDevAuth(t)
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()

	// Message with a past Expiration Time.
	nonce, _, _ := a.IssueNonce()
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	msg := siweMessage("market.example", addr, nonce, past)
	if _, err := a.Authenticate(msg, signSIWE(t, key, msg)); err == nil {
		t.Fatal("expected expired message to be rejected")
	}

	// Expired token.
	token, _ := a.MintToken(addr)
	a.now = func() time.Time { return time.Now().Add(devAuthTokenTTL + time.Minute) }
	if _, err := a.VerifyToken(token); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestDeveloperAuthDomainPinning(t *testing.T) {
	a := NewDeveloperAuth("unit-test-secret", "market.example")
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()

	nonce, _, _ := a.IssueNonce()
	good := siweMessage("market.example", addr, nonce, "")
	if _, err := a.Authenticate(good, signSIWE(t, key, good)); err != nil {
		t.Fatalf("expected pinned domain to pass: %v", err)
	}

	nonce2, _, _ := a.IssueNonce()
	bad := siweMessage("evil.example", addr, nonce2, "")
	if _, err := a.Authenticate(bad, signSIWE(t, key, bad)); err == nil {
		t.Fatal("expected foreign domain to be rejected")
	}
}

// TestDeveloperHeaderRejectedOutsideDevMode locks the trust boundary: the bare
// X-Developer-Wallet header must NOT authenticate in production mode.
func TestDeveloperHeaderRejectedOutsideDevMode(t *testing.T) {
	s := New(Deps{DevMode: false, DeveloperAuthSecret: "unit-test-secret"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/me/services", nil)
	req.Header.Set("X-Developer-Wallet", "0x1111111111111111111111111111111111111111")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bare wallet header in prod mode, got %d", res.StatusCode)
	}
}

// TestDeveloperAuthHTTPFlow exercises nonce -> auth -> token over HTTP.
func TestDeveloperAuthHTTPFlow(t *testing.T) {
	s := New(Deps{DevMode: false, DeveloperAuthSecret: "unit-test-secret"})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/developers/nonce", "application/json", nil)
	if err != nil {
		t.Fatalf("nonce request: %v", err)
	}
	var nonceResp developerNonceResponse
	if err := json.NewDecoder(res.Body).Decode(&nonceResp); err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	res.Body.Close()
	if nonceResp.Nonce == "" {
		t.Fatal("empty nonce")
	}

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()
	msg := siweMessage("market.example", addr, nonceResp.Nonce, "")
	body, _ := json.Marshal(developerAuthRequest{Message: msg, Signature: signSIWE(t, key, msg)})

	res, err = http.Post(srv.URL+"/v1/developers/auth", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("auth request: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth status %d", res.StatusCode)
	}
	var authResp developerAuthResponse
	if err := json.NewDecoder(res.Body).Decode(&authResp); err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	if !strings.EqualFold(authResp.Wallet, addr) || authResp.Token == "" {
		t.Fatalf("unexpected auth response: %+v", authResp)
	}
}

// TestTokenAuthedOwnerRoute proves a SIWE-minted token authenticates an
// owner-scoped route end to end (requires the throwaway test Postgres).
func TestTokenAuthedOwnerRoute(t *testing.T) {
	f := newFixture(t)

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey).Hex()
	if _, err := f.st.UpsertDeveloperByWallet(
		context.Background(), strings.ToLower(addr), strings.ToLower(addr), "Token Dev",
	); err != nil {
		t.Fatalf("upsert developer: %v", err)
	}

	res, body := f.do(t, "POST", "/v1/developers/nonce", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("nonce: status %d body %s", res.StatusCode, body)
	}
	var nonceResp developerNonceResponse
	if err := json.Unmarshal(body, &nonceResp); err != nil {
		t.Fatalf("decode nonce: %v", err)
	}

	msg := siweMessage("market.example", addr, nonceResp.Nonce, "")
	authBody, _ := json.Marshal(developerAuthRequest{Message: msg, Signature: signSIWE(t, key, msg)})
	res, body = f.do(t, "POST", "/v1/developers/auth", string(authBody),
		map[string]string{"Content-Type": "application/json"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("auth: status %d body %s", res.StatusCode, body)
	}
	var authResp developerAuthResponse
	if err := json.Unmarshal(body, &authResp); err != nil {
		t.Fatalf("decode auth: %v", err)
	}

	res, body = f.do(t, "GET", "/v1/me/services", "",
		map[string]string{"X-Developer-Token": authResp.Token})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("me/services with token: status %d body %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "services") {
		t.Fatalf("unexpected body: %s", body)
	}

	// Garbage token must be rejected even in dev mode (token path wins).
	res, _ = f.do(t, "GET", "/v1/me/services", "",
		map[string]string{"X-Developer-Token": "bogus.token"})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for garbage token, got %d", res.StatusCode)
	}
}
