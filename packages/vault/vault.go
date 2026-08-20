package vault

import (
	"context"
	"encoding/hex"
	"sync"
	"time"
)

// Vault ties a KeyProvider (the KEK layer) to per-user key management and object
// sealing. One Vault is shared process-wide; UserVaults are minted per user.
type Vault struct {
	kp  KeyProvider
	now func() int64
}

// New returns a Vault over the given KeyProvider.
func New(kp KeyProvider) *Vault {
	return &Vault{kp: kp, now: func() int64 { return time.Now().Unix() }}
}

func newUKID() (string, error) {
	b, err := randBytes(8)
	if err != nil {
		return "", err
	}
	return "uk-" + hex.EncodeToString(b), nil
}

// ProvisionUser mints a fresh random user key for the DID, wraps it under the
// active KEK, and returns a keyfile holding only the wrapped key. The raw user
// key is never returned or retained; the caller persists the keyfile and later
// OpenUser to obtain a UserVault.
func (v *Vault) ProvisionUser(ctx context.Context, user string) (*UserKeyfile, error) {
	if user == "" {
		return nil, ErrKeyUnavailable
	}
	uk, err := randBytes(keyLen)
	if err != nil {
		return nil, err
	}
	ukID, err := newUKID()
	if err != nil {
		return nil, err
	}
	kekID, err := v.kp.ActiveKEKID(ctx)
	if err != nil {
		return nil, ErrVaultRequired
	}
	wrapped, err := v.kp.Wrap(ctx, kekID, uk, ukWrapAAD(user, ukID))
	if err != nil {
		return nil, err
	}
	return &UserKeyfile{
		Version: 1,
		User:    user,
		Keys: []wrappedUserKey{{
			UKID:    ukID,
			KEKID:   kekID,
			Wrapped: wrapped,
			Created: v.now(),
			Active:  true,
		}},
	}, nil
}

// OpenUser unwraps every user-key version in the keyfile and returns a UserVault
// ready to seal (under the active key) and open (under any retained version).
func (v *Vault) OpenUser(ctx context.Context, kf *UserKeyfile) (*UserVault, error) {
	if kf == nil || kf.User == "" || len(kf.Keys) == 0 {
		return nil, ErrKeyUnavailable
	}
	uv := &UserVault{user: kf.User, uks: make(map[string][]byte, len(kf.Keys))}
	for _, k := range kf.Keys {
		uk, err := v.kp.Unwrap(ctx, k.KEKID, k.Wrapped, ukWrapAAD(kf.User, k.UKID))
		if err != nil {
			return nil, ErrVaultRequired
		}
		if len(uk) != keyLen {
			return nil, ErrKeyUnavailable
		}
		uv.uks[k.UKID] = uk
		if k.Active {
			uv.activeUKID = k.UKID
		}
	}
	if uv.activeUKID == "" {
		return nil, ErrKeyUnavailable
	}
	return uv, nil
}

// RotateKEK re-wraps every user key in the keyfile under the currently active
// KEK, without touching any data object (data DEKs are wrapped by the user key,
// not the KEK). The keyfile is mutated in place.
func (v *Vault) RotateKEK(ctx context.Context, kf *UserKeyfile) error {
	if kf == nil || len(kf.Keys) == 0 {
		return ErrKeyUnavailable
	}
	activeKEK, err := v.kp.ActiveKEKID(ctx)
	if err != nil {
		return ErrVaultRequired
	}
	for i := range kf.Keys {
		k := kf.Keys[i]
		if k.KEKID == activeKEK {
			continue
		}
		uk, err := v.kp.Unwrap(ctx, k.KEKID, k.Wrapped, ukWrapAAD(kf.User, k.UKID))
		if err != nil {
			return ErrVaultRequired
		}
		rewrapped, err := v.kp.Wrap(ctx, activeKEK, uk, ukWrapAAD(kf.User, k.UKID))
		zero(uk)
		if err != nil {
			return err
		}
		kf.Keys[i].KEKID = activeKEK
		kf.Keys[i].Wrapped = rewrapped
	}
	return nil
}

// RotateUK mints a new active user key, demoting existing versions to
// read-only. Data written under an old version stays readable because the old
// wrapped keys are retained. The keyfile is mutated in place.
func (v *Vault) RotateUK(ctx context.Context, kf *UserKeyfile) error {
	if kf == nil || kf.User == "" {
		return ErrKeyUnavailable
	}
	uk, err := randBytes(keyLen)
	if err != nil {
		return err
	}
	defer zero(uk)
	ukID, err := newUKID()
	if err != nil {
		return err
	}
	kekID, err := v.kp.ActiveKEKID(ctx)
	if err != nil {
		return ErrVaultRequired
	}
	wrapped, err := v.kp.Wrap(ctx, kekID, uk, ukWrapAAD(kf.User, ukID))
	if err != nil {
		return err
	}
	for i := range kf.Keys {
		kf.Keys[i].Active = false
	}
	kf.Keys = append(kf.Keys, wrappedUserKey{
		UKID:    ukID,
		KEKID:   kekID,
		Wrapped: wrapped,
		Created: v.now(),
		Active:  true,
	})
	return nil
}

// UserVault seals and opens objects for one user. It holds the unwrapped active
// user key for new writes and every retained version for reads. It is safe for
// concurrent use.
type UserVault struct {
	user       string
	mu         sync.RWMutex
	activeUKID string
	uks        map[string][]byte
}

// User returns the DID this vault is bound to.
func (uv *UserVault) User() string { return uv.user }

func (uv *UserVault) uk(id string) ([]byte, bool) {
	uv.mu.RLock()
	defer uv.mu.RUnlock()
	k, ok := uv.uks[id]
	return k, ok
}

// sealSingle encrypts plaintext as a single-object shape (record or file): a
// fresh DEK wrapped under the active user key, then the payload sealed under the
// DEK with the header+AD bound as associated data.
func (uv *UserVault) sealSingle(shape Shape, ad AD, plaintext []byte) ([]byte, error) {
	uv.mu.RLock()
	activeID := uv.activeUKID
	activeUK := uv.uks[activeID]
	uv.mu.RUnlock()
	if activeUK == nil {
		return nil, ErrKeyUnavailable
	}
	dek, err := randBytes(keyLen)
	if err != nil {
		return nil, err
	}
	defer zero(dek)
	wrappedDEK, err := seal(activeUK, dek, dekWrapAAD)
	if err != nil {
		return nil, err
	}
	h := Header{Format: FormatVersion, ADSchema: ADSchemaV1, Shape: shape, UKID: activeID, WrappedDEK: wrappedDEK}
	hb := h.marshal()
	aad := append(append([]byte(nil), hb...), ad.encode(ADSchemaV1)...)
	body, err := seal(dek, plaintext, aad)
	if err != nil {
		return nil, err
	}
	return append(hb, body...), nil
}

// openSingle reverses sealSingle, requiring the header shape to match want.
func (uv *UserVault) openSingle(want Shape, ad AD, obj []byte) ([]byte, error) {
	h, n, err := unmarshalHeader(obj)
	if err != nil {
		return nil, err
	}
	if h.Shape != want {
		return nil, ErrUnsupported
	}
	uk, ok := uv.uk(h.UKID)
	if !ok {
		return nil, ErrKeyUnavailable
	}
	dek, err := open(uk, h.WrappedDEK, dekWrapAAD)
	if err != nil {
		return nil, ErrAuth
	}
	defer zero(dek)
	hb := obj[:n]
	aad := append(append([]byte(nil), hb...), ad.encode(h.ADSchema)...)
	return open(dek, obj[n:], aad)
}

// SealRecord encrypts one JSONL record / Pebble value as an independent AEAD
// object. AD binds the record to its user, store, stream, sequence, and schema.
func (uv *UserVault) SealRecord(ad AD, plaintext []byte) ([]byte, error) {
	return uv.sealSingle(ShapeRecord, ad, plaintext)
}

// OpenRecord decrypts a record sealed by SealRecord under the reconstructed AD.
func (uv *UserVault) OpenRecord(ad AD, obj []byte) ([]byte, error) {
	return uv.openSingle(ShapeRecord, ad, obj)
}

// SealFile encrypts a whole atomic JSON file. The caller writes the result via
// tmp+rename exactly as it wrote plaintext.
func (uv *UserVault) SealFile(ad AD, plaintext []byte) ([]byte, error) {
	return uv.sealSingle(ShapeFile, ad, plaintext)
}

// OpenFile decrypts a whole-file object sealed by SealFile.
func (uv *UserVault) OpenFile(ad AD, obj []byte) ([]byte, error) {
	return uv.openSingle(ShapeFile, ad, obj)
}

// zero overwrites a key buffer.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
