package vault

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Config describes how a daemon should bring up its vault at boot. A per-user
// daemon holds exactly one user key, so one Config yields one Session.
type Config struct {
	// Required is the fail-closed switch. When true (production default) the
	// vault MUST come up with a usable key or Boot refuses; no plaintext
	// fallback exists. When false, a missing provider yields a dev/CLI plaintext
	// Session.
	Required bool
	// DataDir is the daemon's data root; the wrapped keyfile lives beside it.
	DataDir string
	// UserDID identifies the user this daemon serves; it names the minted key.
	UserDID string
	// Provider, when non-nil, is used directly (production injects a KMS-backed
	// provider here). When nil, Boot resolves a dev provider from KEKHex/KEKFile.
	Provider KeyProvider
	// KEKHex is a hex-encoded 32-byte KEK for dev/CLI use.
	KEKHex string
	// KEKFile is a path to a file holding the KEK (32 raw bytes or hex text).
	KEKFile string
	// KEKID names the KEK; empty defaults to "kek1".
	KEKID string
}

// Env var names the daemon reads to configure the vault.
const (
	EnvRequired = "VAULT_REQUIRED"
	EnvKEKHex   = "VAULT_KEK"
	EnvKEKFile  = "VAULT_KEK_FILE"
	EnvKEKID    = "VAULT_KEK_ID"
)

// ConfigFromEnv builds a Config from the environment for the given data dir and
// user DID. The requirement is opt-in: the platform makes production fail-closed
// by injecting VAULT_REQUIRED=true into the per-user machine alongside a
// provisioned KEK (see the router MachineEnv wiring), so a machine without key
// material never bricks on boot while an enabled machine never falls back to
// plaintext.
func ConfigFromEnv(dataDir, userDID string) Config {
	return Config{
		Required: envRequired(os.Getenv(EnvRequired)),
		DataDir:  dataDir,
		UserDID:  userDID,
		KEKHex:   os.Getenv(EnvKEKHex),
		KEKFile:  os.Getenv(EnvKEKFile),
		KEKID:    os.Getenv(EnvKEKID),
	}
}

// envRequired enforces the vault only when explicitly enabled. Production sets a
// truthy VAULT_REQUIRED via the platform; an absent/typo'd value is treated as
// not-required so an un-provisioned machine boots (plaintext dev) rather than
// refusing — the fail-closed guarantee binds once the platform turns it on.
func envRequired(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Session is the outcome of Boot: either an encrypting session (UserVault set)
// or an explicitly-disabled plaintext session (dev/CLI only).
type Session struct {
	uv       *UserVault
	required bool
}

// Encrypting reports whether writes will be sealed.
func (s *Session) Encrypting() bool { return s != nil && s.uv != nil }

// UserVault returns the underlying vault, or nil in plaintext mode.
func (s *Session) UserVault() *UserVault {
	if s == nil {
		return nil
	}
	return s.uv
}

// MaybeSealRecord seals a JSONL record when encrypting, returns the plaintext
// unchanged when running in an explicitly-disabled dev session, and returns a
// hard error when the vault is required but no key is available — never a
// silent plaintext fallback.
func (s *Session) MaybeSealRecord(ad AD, plaintext []byte) ([]byte, error) {
	if s != nil && s.uv != nil {
		return s.uv.SealRecord(ad, plaintext)
	}
	if s != nil && s.required {
		return nil, ErrVaultRequired
	}
	return plaintext, nil
}

// MaybeSealFile is the whole-file analogue of MaybeSealRecord.
func (s *Session) MaybeSealFile(ad AD, plaintext []byte) ([]byte, error) {
	if s != nil && s.uv != nil {
		return s.uv.SealFile(ad, plaintext)
	}
	if s != nil && s.required {
		return nil, ErrVaultRequired
	}
	return plaintext, nil
}

// KeyfilePath is where the wrapped per-user keyfile lives for a data dir.
func KeyfilePath(dataDir string) string { return filepath.Join(dataDir, "vault.key") }

// Boot brings up the vault for a per-user daemon. Its contract is fail-closed:
//   - Provider available: load an existing wrapped keyfile (or provision one),
//     open the UserVault, return an encrypting Session.
//   - Provider unavailable and Required: refuse (ErrVaultRequired) — the daemon
//     must not boot into plaintext mode.
//   - Provider unavailable and not Required: return a plaintext dev Session.
//
// A keyfile that exists but cannot be unwrapped (wrong/rotated-away KEK) is a
// hard boot refusal when Required, never a silent re-provision.
func Boot(ctx context.Context, cfg Config) (*Session, error) {
	kp, err := resolveProvider(cfg)
	if err != nil {
		if cfg.Required {
			return nil, ErrVaultRequired
		}
		return &Session{required: false}, nil
	}
	if kp == nil {
		if cfg.Required {
			return nil, ErrVaultRequired
		}
		return &Session{required: false}, nil
	}
	if cfg.DataDir == "" || cfg.UserDID == "" {
		return nil, ErrVaultRequired
	}
	v := New(kp)
	uv, err := loadOrProvision(ctx, v, cfg)
	if err != nil {
		// Fail closed: a required vault that cannot open its key refuses to boot.
		if cfg.Required {
			return nil, ErrVaultRequired
		}
		return nil, err
	}
	return &Session{uv: uv, required: cfg.Required}, nil
}

func loadOrProvision(ctx context.Context, v *Vault, cfg Config) (*UserVault, error) {
	path := KeyfilePath(cfg.DataDir)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		kf, perr := ParseKeyfile(b)
		if perr != nil {
			return nil, perr
		}
		if kf.User != cfg.UserDID {
			// The keyfile belongs to a different user — refuse rather than
			// silently mint a second key over another user's data.
			return nil, ErrKeyUnavailable
		}
		return v.OpenUser(ctx, kf)
	case os.IsNotExist(err):
		kf, perr := v.ProvisionUser(ctx, cfg.UserDID)
		if perr != nil {
			return nil, perr
		}
		if perr := writeKeyfile(path, kf); perr != nil {
			return nil, perr
		}
		return v.OpenUser(ctx, kf)
	default:
		return nil, err
	}
}

func writeKeyfile(path string, kf *UserKeyfile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := kf.Marshal()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolveProvider returns the configured KeyProvider, or (nil, nil) when no dev
// key material is present (so the caller applies the Required policy). An
// injected Provider takes precedence over env-derived dev keys.
func resolveProvider(cfg Config) (KeyProvider, error) {
	if cfg.Provider != nil {
		return cfg.Provider, nil
	}
	kekID := cfg.KEKID
	if kekID == "" {
		kekID = "kek1"
	}
	var kek []byte
	switch {
	case cfg.KEKHex != "":
		k, err := decodeKEK(cfg.KEKHex)
		if err != nil {
			return nil, err
		}
		kek = k
	case cfg.KEKFile != "":
		raw, err := os.ReadFile(cfg.KEKFile)
		if err != nil {
			return nil, err
		}
		k, err := parseKEKBytes(raw)
		if err != nil {
			return nil, err
		}
		kek = k
	default:
		return nil, nil
	}
	return NewStaticKeyProvider(map[string][]byte{kekID: kek}, kekID)
}

func decodeKEK(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(b) != keyLen {
		return nil, ErrKeyUnavailable
	}
	return b, nil
}

// parseKEKBytes accepts a keyfile that is either 32 raw bytes or hex text.
func parseKEKBytes(raw []byte) ([]byte, error) {
	if len(raw) == keyLen {
		return raw, nil
	}
	return decodeKEK(string(raw))
}
