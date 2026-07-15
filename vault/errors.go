package vault

import "errors"

// ErrAuth is the single, indistinguishable error returned whenever an AEAD open
// fails — whether the ciphertext was tampered with, the wrong user key was used,
// or the object is otherwise not authentic. Callers must not be able to
// distinguish disclosure from tampering (NIST SP 800-38D authenticity).
var ErrAuth = errors.New("vault: authentication failed")

// ErrNotVault is returned by decoders when the bytes carry no vault header
// (magic mismatch). Sniffing readers use it to fall through to legacy plaintext.
var ErrNotVault = errors.New("vault: not a vault object")

// ErrTruncated is returned when a ciphertext object is shorter than its header
// or framing implies, or when a stream ends before its final chunk.
var ErrTruncated = errors.New("vault: truncated ciphertext")

// ErrUnsupported is returned for an unknown format version, shape, or AD schema.
var ErrUnsupported = errors.New("vault: unsupported format")

// ErrKeyUnavailable is returned when a KeyProvider cannot produce the requested
// KEK, or a user key version referenced by an object is not loaded.
var ErrKeyUnavailable = errors.New("vault: key unavailable")

// ErrVaultRequired is returned by fail-closed call sites when the vault is
// mandatory (production) but no usable key material is present.
var ErrVaultRequired = errors.New("vault: required but unavailable")
