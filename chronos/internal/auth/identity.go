// Package auth implements chronosd's two principal-auth primitives: the
// agent-DID ed25519 challenge/verify lane (proves WHICH agent/owner is posting)
// and the short-lived HMAC principal token minted on a successful verify.
//
// This mirrors the live wallet lane (protocol/paxeer-embeded-wallets) and the
// UWAC identity package: the daemon's executor key IS the agent identity, and
// the DID label IS the owner's Supabase user UUID (did:matrix:<MATRIX_USER_ID>:
// <keyfp>). chronosd owns BOTH sides of its own challenge (chronos.mjs signs,
// chronosd verifies), so the challenge message format is defined here and must
// match the proxy.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DID is a parsed did:matrix identity.
type DID struct {
	Raw   string
	Label string
	KeyFP string // hex(pubkey)[:16]
}

var (
	didRe  = regexp.MustCompile(`^did:matrix:([^:]+):([0-9a-fA-F]{16})$`)
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ParseDID parses a did:matrix:<label>:<16-hex-fingerprint>.
func ParseDID(s string) (DID, error) {
	m := didRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return DID{}, fmt.Errorf("auth: malformed did %q", s)
	}
	return DID{Raw: strings.TrimSpace(s), Label: m[1], KeyFP: strings.ToLower(m[2])}, nil
}

// IsUUID reports whether s is a canonical UUID (the Supabase user id shape).
func IsUUID(s string) bool { return uuidRe.MatchString(strings.TrimSpace(s)) }

// OwnerFromDID returns the owner Supabase user id when the DID label is a UUID
// (the AGENT_BIND_OWNER_FROM_DID convention). Otherwise it falls back to the
// raw label so non-UUID labels (e.g. dev "executor") still route deterministically.
func OwnerFromDID(d DID) string {
	if IsUUID(d.Label) {
		return strings.ToLower(d.Label)
	}
	return d.Label
}

// ChallengeMessage is the exact UTF-8 string the agent must ed25519-sign.
// MUST stay in lockstep with tools/chronos/chronos.mjs.
func ChallengeMessage(did, nonce string) string {
	return "matrix-chronos-auth:" + did + ":" + nonce
}

// VerifySignature checks the ed25519 signature over ChallengeMessage(did,nonce)
// AND that the supplied public key matches the fingerprint embedded in the DID
// (so a caller cannot present an unrelated key for a known DID).
func VerifySignature(didStr, pubHex, nonce, sigHex string) error {
	d, err := ParseDID(didStr)
	if err != nil {
		return err
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(pubHex, "0x"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("auth: invalid public key")
	}
	if hex.EncodeToString(pub)[:16] != d.KeyFP {
		return errors.New("auth: public key does not match did fingerprint")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil {
		return errors.New("auth: invalid signature encoding")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(ChallengeMessage(didStr, nonce)), sig) {
		return errors.New("auth: signature verification failed")
	}
	return nil
}

// Challenges is an in-memory single-use nonce store with TTL.
type Challenges struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]entry
}

type entry struct {
	did string
	exp time.Time
}

// NewChallenges constructs a challenge store with the given nonce TTL.
func NewChallenges(ttl time.Duration) *Challenges {
	return &Challenges{ttl: ttl, m: map[string]entry{}}
}

// TTL returns the configured nonce time-to-live.
func (c *Challenges) TTL() time.Duration { return c.ttl }

// Create issues a fresh nonce bound to did and returns the nonce + the exact
// message to sign.
func (c *Challenges) Create(did string) (nonce, message string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	nonce = base64.RawURLEncoding.EncodeToString(b)
	c.mu.Lock()
	c.m[nonce] = entry{did: did, exp: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return nonce, ChallengeMessage(did, nonce)
}

// Consume atomically validates + deletes a nonce (single-use). It returns false
// for unknown, expired, already-used, or DID-mismatched nonces.
func (c *Challenges) Consume(nonce, did string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[nonce]
	if !ok {
		return false
	}
	delete(c.m, nonce)
	return e.did == did && time.Now().Before(e.exp)
}

// Purge drops expired challenges (opportunistic GC).
func (c *Challenges) Purge() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.m {
		if now.After(e.exp) {
			delete(c.m, k)
		}
	}
}
