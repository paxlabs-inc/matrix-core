// Package sig provides the sequencer's ed25519 receipt-signing key. The
// sequencer signs every inclusion receipt so an agent can prove the sequencer
// attested to its transfer (and detect equivocation). This is the sequencer's
// OWN key — distinct from any agent DID key and from the on-chain operator key
// (layerx.frozen.kvx [principles] key_isolation).
package sig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// Signer holds the sequencer's ed25519 keypair.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// New builds a Signer from a 64-hex ed25519 SEED. An empty seed generates an
// ephemeral key (dev only — receipts won't verify across restarts).
func New(seedHex string) (*Signer, bool, error) {
	seedHex = strings.TrimPrefix(strings.TrimSpace(seedHex), "0x")
	if seedHex == "" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, false, err
		}
		return &Signer{priv: priv, pub: pub}, true, nil
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false, errors.New("sig: LAYERX_SEQUENCER_KEY must be 64-hex (32-byte ed25519 seed)")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, false, nil
}

// Sign returns hex(ed25519 signature) over msg.
func (s *Signer) Sign(msg []byte) string {
	return hex.EncodeToString(ed25519.Sign(s.priv, msg))
}

// PublicHex returns the hex-encoded public key (stamped on every receipt).
func (s *Signer) PublicHex() string { return hex.EncodeToString(s.pub) }
