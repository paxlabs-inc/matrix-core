package workforceauth

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

const minimumRootSecretBytes = 32

// Deriver expands one router-only root secret into domain-separated,
// per-user Workforce credentials. The root secret is never copied into a
// provisioned environment.
type Deriver struct {
	root []byte
}

func New(root string) (*Deriver, error) {
	if len([]byte(root)) < minimumRootSecretBytes {
		return nil, fmt.Errorf("workforce credentials: root secret must contain at least %d bytes", minimumRootSecretBytes)
	}
	return &Deriver{root: append([]byte(nil), []byte(root)...)}, nil
}

func (d *Deriver) OwnerToken(userID string) (string, error) {
	return d.token("owner-token", userID)
}

func (d *Deriver) WakeToken(userID string) (string, error) {
	return d.token("wake-token", userID)
}

func (d *Deriver) RuntimePrivateKey(userID string) (string, error) {
	seed, err := d.derive("runtime-signing-key", userID)
	if err != nil {
		return "", err
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	return base64.RawURLEncoding.EncodeToString(privateKey), nil
}

func (d *Deriver) CompanyIssuerPrivateKey(userID string) (string, error) {
	seed, err := d.derive("company-issuer-signing-key", userID)
	if err != nil {
		return "", err
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	return base64.RawURLEncoding.EncodeToString(privateKey), nil
}

func (d *Deriver) BootstrapOwnerPublicKey(userID string) (string, error) {
	seed, err := d.derive("bootstrap-owner-key", userID)
	if err != nil {
		return "", err
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(publicKey), nil
}

func (d *Deriver) token(purpose, userID string) (string, error) {
	value, err := d.derive(purpose, userID)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (d *Deriver) derive(purpose, userID string) ([]byte, error) {
	if d == nil || len(d.root) < minimumRootSecretBytes {
		return nil, fmt.Errorf("workforce credentials: deriver is unavailable")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" || len(userID) > 128 {
		return nil, fmt.Errorf("workforce credentials: valid user id is required")
	}
	mac := hmac.New(sha256.New, d.root)
	_, _ = mac.Write([]byte("matrix-workforce-v1\x00"))
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte{'\x00'})
	_, _ = mac.Write([]byte(userID))
	return mac.Sum(nil), nil
}
