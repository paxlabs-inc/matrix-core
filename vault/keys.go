package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// dekWrapAAD is a constant domain-separation label bound when wrapping a DEK
// under a user key.
var dekWrapAAD = []byte("MXV1|dek")

// KeyProvider is the seam over the platform key-encryption key (KEK). Dev builds
// use a file/env provider; production injects a remote-KMS-backed provider of
// the same shape. It wraps and unwraps user keys under a KEK identified by id,
// so that KEK rotation re-wraps user keys without touching any data object.
//
// Wrap and Unwrap take explicit associated data so a wrapped user key is bound
// to its user and version and cannot be relabeled onto another user.
type KeyProvider interface {
	// ActiveKEKID returns the id of the KEK new wraps should use.
	ActiveKEKID(ctx context.Context) (string, error)
	// Wrap encrypts plaintext under the KEK identified by kekID, binding aad.
	Wrap(ctx context.Context, kekID string, plaintext, aad []byte) ([]byte, error)
	// Unwrap reverses Wrap for the KEK identified by kekID.
	Unwrap(ctx context.Context, kekID string, wrapped, aad []byte) ([]byte, error)
}

// StaticKeyProvider is a KEK provider backed by an in-memory set of KEKs, minted
// from a file or environment for dev/CLI use. Production replaces it with a
// remote-KMS provider of the same shape (see KMSKeyProvider).
type StaticKeyProvider struct {
	mu     sync.RWMutex
	keks   map[string][]byte
	active string
}

// NewStaticKeyProvider builds a provider from a map of kekID->32-byte key with a
// designated active id. It copies the key bytes so the caller's slices are not
// retained.
func NewStaticKeyProvider(keks map[string][]byte, active string) (*StaticKeyProvider, error) {
	if len(keks) == 0 {
		return nil, ErrKeyUnavailable
	}
	if _, ok := keks[active]; !ok {
		return nil, ErrKeyUnavailable
	}
	cp := make(map[string][]byte, len(keks))
	for id, k := range keks {
		if len(k) != keyLen {
			return nil, ErrKeyUnavailable
		}
		cp[id] = append([]byte(nil), k...)
	}
	return &StaticKeyProvider{keks: cp, active: active}, nil
}

func (p *StaticKeyProvider) ActiveKEKID(ctx context.Context) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active == "" {
		return "", ErrKeyUnavailable
	}
	return p.active, nil
}

func (p *StaticKeyProvider) kek(id string) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	k, ok := p.keks[id]
	if !ok {
		return nil, ErrKeyUnavailable
	}
	return k, nil
}

func (p *StaticKeyProvider) Wrap(ctx context.Context, kekID string, plaintext, aad []byte) ([]byte, error) {
	k, err := p.kek(kekID)
	if err != nil {
		return nil, err
	}
	return seal(k, plaintext, aad)
}

func (p *StaticKeyProvider) Unwrap(ctx context.Context, kekID string, wrapped, aad []byte) ([]byte, error) {
	k, err := p.kek(kekID)
	if err != nil {
		return nil, err
	}
	return open(k, wrapped, aad)
}

// AddKEK installs an additional KEK and makes it active, for testing rotation.
// The prior KEKs remain available for unwrapping existing wrapped user keys.
func (p *StaticKeyProvider) AddKEK(id string, key []byte, makeActive bool) error {
	if len(key) != keyLen {
		return ErrKeyUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keks[id] = append([]byte(nil), key...)
	if makeActive {
		p.active = id
	}
	return nil
}

// KMS is the minimal remote key service a production KeyProvider delegates to.
// The concrete client (AWS KMS, GCP KMS, Vault Transit, ...) is out of scope for
// this module — only the seam shape is defined here.
type KMS interface {
	Encrypt(ctx context.Context, keyID string, plaintext, aad []byte) ([]byte, error)
	Decrypt(ctx context.Context, keyID string, ciphertext, aad []byte) ([]byte, error)
	ActiveKeyID(ctx context.Context) (string, error)
}

// KMSKeyProvider adapts a remote KMS into the KeyProvider seam. Wrap/Unwrap are
// remote envelope operations; the KEK never leaves the KMS.
type KMSKeyProvider struct{ kms KMS }

// NewKMSKeyProvider returns a provider backed by kms, or ErrKeyUnavailable if
// kms is nil (fail closed — a production provider must have a real KMS).
func NewKMSKeyProvider(kms KMS) (*KMSKeyProvider, error) {
	if kms == nil {
		return nil, ErrKeyUnavailable
	}
	return &KMSKeyProvider{kms: kms}, nil
}

func (p *KMSKeyProvider) ActiveKEKID(ctx context.Context) (string, error) {
	return p.kms.ActiveKeyID(ctx)
}

func (p *KMSKeyProvider) Wrap(ctx context.Context, kekID string, plaintext, aad []byte) ([]byte, error) {
	return p.kms.Encrypt(ctx, kekID, plaintext, aad)
}

func (p *KMSKeyProvider) Unwrap(ctx context.Context, kekID string, wrapped, aad []byte) ([]byte, error) {
	return p.kms.Decrypt(ctx, kekID, wrapped, aad)
}

// wrappedUserKey is one version of a user key, stored only in KEK-wrapped form.
type wrappedUserKey struct {
	UKID    string `json:"uk_id"`
	KEKID   string `json:"kek_id"`
	Wrapped []byte `json:"wrapped"`
	Created int64  `json:"created"`
	Active  bool   `json:"active"`
}

// UserKeyfile is the durable, per-user key record. It holds only wrapped user
// keys — never raw key bytes — and is safe to persist beside the user's data
// (the KEK, held by the KeyProvider, protects it). Multiple versions coexist so
// data written under a rotated-out UK stays readable.
type UserKeyfile struct {
	Version int              `json:"version"`
	User    string           `json:"user"`
	Keys    []wrappedUserKey `json:"keys"`
}

func ukWrapAAD(user, ukID string) []byte {
	return []byte(fmt.Sprintf("MXV1|uk|%s|%s", user, ukID))
}

// activeUK returns the active wrapped key entry, or false if none.
func (kf *UserKeyfile) active() (wrappedUserKey, bool) {
	for _, k := range kf.Keys {
		if k.Active {
			return k, true
		}
	}
	return wrappedUserKey{}, false
}

// Marshal serializes the keyfile to JSON. Only wrapped key bytes are present.
func (kf *UserKeyfile) Marshal() ([]byte, error) { return json.Marshal(kf) }

// ParseKeyfile parses a keyfile from JSON.
func ParseKeyfile(b []byte) (*UserKeyfile, error) {
	var kf UserKeyfile
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, err
	}
	return &kf, nil
}
