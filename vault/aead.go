package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
)

// keyLen is the AES-256 key length. All keys in the hierarchy (KEK, UK, DEK) are
// 256-bit.
const keyLen = 32

// nonceLen is the AES-GCM nonce length: 96 bits per NIST SP 800-38D.
const nonceLen = 12

// newGCM builds an AES-256-GCM AEAD from a 32-byte key. A non-32-byte key is a
// programming error and surfaces as ErrKeyUnavailable rather than leaking the
// key length elsewhere.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, ErrKeyUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrKeyUnavailable
	}
	return cipher.NewGCM(block)
}

// randBytes returns n cryptographically random bytes.
func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}

// seal encrypts plaintext with key under a fresh random nonce, binding aad, and
// returns nonce||ciphertext (the GCM tag is appended by Seal).
func seal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce, err := randBytes(nonceLen)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, nonceLen+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// open decrypts a nonce||ciphertext blob produced by seal. Any failure — wrong
// key, tampered bytes, tampered aad — collapses to ErrAuth.
func open(key, blob, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < nonceLen+aead.Overhead() {
		return nil, ErrTruncated
	}
	nonce := blob[:nonceLen]
	ct := blob[nonceLen:]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrAuth
	}
	return pt, nil
}
