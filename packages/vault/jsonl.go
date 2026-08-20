package vault

import "encoding/base64"

// JSONL record line codec. A sealed record is binary (it can contain any byte,
// including newlines), so it cannot live verbatim on a line of a newline-
// delimited file. This codec base64-encodes a sealed record with the raw
// standard alphabet — no padding and no newline — so each encrypted record is
// exactly one line of a .jsonl file, preserving O(1) O_APPEND appends and
// skip-torn-tail crash recovery.
//
// Reads sniff each line: a line whose base64 decodes to a vault object (magic
// present) is decrypted under the reconstructed associated data; every other
// line is returned unchanged as legacy plaintext. A legacy JSON record always
// begins with '{' (0x7B), which is not in the base64 alphabet, so its decode
// fails fast and it is read exactly as today until the user is migrated.
var recLineEnc = base64.RawStdEncoding

// EncodeLine encodes one JSONL record for storage.
//
//   - Encrypting session: the record is sealed under ad, then base64-encoded so
//     it is newline-free and can be appended as a single line.
//   - Explicitly-disabled dev session: the plaintext is returned unchanged
//     (legacy-compatible on-disk shape).
//   - Vault required but no key: a hard error, never a silent plaintext write.
func (s *Session) EncodeLine(ad AD, plaintext []byte) ([]byte, error) {
	if s != nil && s.uv != nil {
		sealed, err := s.uv.SealRecord(ad, plaintext)
		if err != nil {
			return nil, err
		}
		enc := make([]byte, recLineEnc.EncodedLen(len(sealed)))
		recLineEnc.Encode(enc, sealed)
		return enc, nil
	}
	if s != nil && s.required {
		return nil, ErrVaultRequired
	}
	return plaintext, nil
}

// DecodeLine decodes one JSONL record line. A line that decodes to a vault
// object is decrypted under the reconstructed ad; any other line is returned
// unchanged as legacy plaintext, so a store mid-migration reads both shapes. A
// vault line with no usable key yields a hard error rather than leaking bytes.
func (s *Session) DecodeLine(ad AD, line []byte) ([]byte, error) {
	if dec, ok := decodeVaultLine(line); ok {
		if s == nil || s.uv == nil {
			return nil, ErrKeyUnavailable
		}
		return s.uv.OpenRecord(ad, dec)
	}
	return line, nil
}

// IsSealedLine reports whether a JSONL line is a vault-sealed record (as opposed
// to legacy plaintext). Migration and the plaintext scanner use it.
func IsSealedLine(line []byte) bool {
	_, ok := decodeVaultLine(line)
	return ok
}

// decodeVaultLine base64-decodes a line and reports whether it is a vault
// object. A non-base64 line (legacy JSON) or a decoded blob without the vault
// magic returns (nil, false).
func decodeVaultLine(line []byte) ([]byte, bool) {
	if len(line) == 0 {
		return nil, false
	}
	dec := make([]byte, recLineEnc.DecodedLen(len(line)))
	n, err := recLineEnc.Decode(dec, line)
	if err != nil || !IsVault(dec[:n]) {
		return nil, false
	}
	return dec[:n], true
}
