package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultAPIAddr      = ":8645"
	defaultProjectRoot  = "."
	defaultArtifactsDir = "artifacts"
	defaultRegistryPath = "registry.json"
	defaultConfigPath   = "tachyon.config.kvx"

	// WalletModeSelfHosted signs locally with a key/keystore the operator owns.
	WalletModeSelfHosted = "self_hosted"
	// WalletModeEmbedded delegates signing to the Paxeer embedded wallet via the
	// agent-native DID lane (no keys held locally).
	WalletModeEmbedded = "embedded"

	// SignerRaw reads a raw hex private key (last resort; file must be 0600).
	SignerRaw = "raw"
	// SignerKeystore decrypts a web3 secret-storage (geth v3) keystore JSON.
	SignerKeystore = "keystore"
	// SignerEnv reads the private key from an environment variable reference.
	SignerEnv = "env"
)

// Config holds daemon configuration. Precedence: environment > tachyon.config.kvx > defaults.
type Config struct {
	APIAddr      string
	ProjectRoot  string
	ArtifactsDir string
	RegistryPath string
	SolcPath     string
	ForgePath    string

	// AuthToken, when set, requires a matching Bearer token on every request
	// except GET /healthz and GET /.
	AuthToken string

	Wallet   WalletConfig
	Policies map[string]PolicyProfile
	Chains   []ChainConfig
}

// WalletConfig selects and configures the signing backend.
type WalletConfig struct {
	Mode string // "" | self_hosted | embedded

	// self_hosted
	Signer           string // raw | keystore | env
	PrivateKey       string // SignerRaw / SignerEnv (resolved value, never logged)
	KeystorePath     string // SignerKeystore
	KeystorePassword string // SignerKeystore

	// embedded (agent-native DID lane)
	Keyfile string // path to the ed25519 seed (64-hex) used as the agent identity
	Label   string // DID label, e.g. "executor"
	API     string // embedded wallet base URL
}

// PolicyProfile is a named capability profile referenced by capability_token.
type PolicyProfile struct {
	Name        string
	SpendCapWei string
	Allow       []string
	Chains      []string
}

// ChainConfig is an operator-defined chain profile from the kvx file.
type ChainConfig struct {
	ID       string
	Name     string
	RPCURL   string
	ChainID  uint64
	Explorer string
}

// Load reads configuration with precedence environment > tachyon.config.kvx > defaults.
// A .env file in the working directory is loaded when present (KEY=VALUE lines only).
func Load() (Config, error) {
	_ = loadDotEnv(".env")

	configPath := envOr("TACHYON_CONFIG", defaultConfigPath)
	doc, _, err := parseKVXFile(configPath)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		APIAddr:      pick("TACHYON_API_ADDR", doc.str("server", "addr"), defaultAPIAddr),
		ProjectRoot:  pick("TACHYON_PROJECT_ROOT", doc.str("server", "project_root"), defaultProjectRoot),
		ArtifactsDir: pick("TACHYON_ARTIFACTS_DIR", doc.str("server", "artifacts_dir"), defaultArtifactsDir),
		RegistryPath: pick("TACHYON_REGISTRY_PATH", doc.str("server", "registry_path"), defaultRegistryPath),
		SolcPath:     pick("TACHYON_SOLC_PATH", doc.str("server", "solc_path"), ""),
		ForgePath:    pick("TACHYON_FORGE_PATH", doc.str("server", "forge_path"), "forge"),
		AuthToken:    pick("TACHYON_AUTH_TOKEN", doc.str("server", "auth_token"), ""),
		Wallet:       loadWallet(doc),
		Policies:     loadPolicies(doc),
		Chains:       loadChains(doc),
	}

	if abs, err := os.Getwd(); err == nil && cfg.ProjectRoot == "." {
		cfg.ProjectRoot = abs
	}
	if root := findFoundryRoot(cfg.ProjectRoot); root != "" {
		cfg.ProjectRoot = root
	}

	return cfg, nil
}

func loadWallet(doc *kvxDoc) WalletConfig {
	w := WalletConfig{
		Mode: pick("TACHYON_WALLET_MODE", doc.str("wallet", "mode"), ""),
	}
	// Backwards-compatible env shims for the original dev/Paxeer signers.
	if w.Mode == "" {
		if strings.EqualFold(os.Getenv("TACHYON_ALLOW_DEV_SIGNER"), "true") || os.Getenv("TACHYON_DEV_PRIVATE_KEY") != "" {
			w.Mode = WalletModeSelfHosted
		} else if os.Getenv("PAXEER_WALLET_TOKEN") != "" || os.Getenv("MATRIX_EXECUTOR_KEY") != "" {
			w.Mode = WalletModeEmbedded
		}
	}

	w.Signer = pick("TACHYON_WALLET_SIGNER", doc.str("wallet.self_hosted", "signer"), "")
	w.PrivateKey = pick("TACHYON_DEV_PRIVATE_KEY", doc.str("wallet.self_hosted", "private_key"), "")
	w.KeystorePath = pick("TACHYON_KEYSTORE_PATH", doc.str("wallet.self_hosted", "keystore_path"), "")
	w.KeystorePassword = pick("TACHYON_KEYSTORE_PASSWORD", doc.str("wallet.self_hosted", "keystore_password"), "")
	if w.Signer == "" && w.PrivateKey != "" {
		w.Signer = SignerRaw
	}
	if w.Signer == "" && w.KeystorePath != "" {
		w.Signer = SignerKeystore
	}

	w.Keyfile = pick("MATRIX_EXECUTOR_KEY", doc.str("wallet.embedded", "keyfile"), "")
	w.Label = pick("TACHYON_AGENT_LABEL", doc.str("wallet.embedded", "label"), "executor")
	w.API = pick("PAXEER_WALLET_API", doc.str("wallet.embedded", "api"), "")
	return w
}

func loadPolicies(doc *kvxDoc) map[string]PolicyProfile {
	out := map[string]PolicyProfile{}
	for _, name := range doc.sectionsWithPrefix("policy") {
		sec := "policy." + name
		out[name] = PolicyProfile{
			Name:        name,
			SpendCapWei: doc.str(sec, "spend_cap_wei"),
			Allow:       doc.list(sec, "allow"),
			Chains:      doc.list(sec, "chains"),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func loadChains(doc *kvxDoc) []ChainConfig {
	var out []ChainConfig
	for _, id := range doc.sectionsWithPrefix("chains") {
		sec := "chains." + id
		out = append(out, ChainConfig{
			ID:       id,
			Name:     doc.str(sec, "name"),
			RPCURL:   doc.str(sec, "rpc_url"),
			ChainID:  doc.uint64Or(sec, "chain_id", 0),
			Explorer: doc.str(sec, "explorer"),
		})
	}
	return out
}

// pick returns the first non-empty of: environment value, kvx value, fallback.
func pick(envKey, kvxVal, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	if strings.TrimSpace(kvxVal) != "" {
		return kvxVal
	}
	return fallback
}

func findFoundryRoot(start string) string {
	dir := start
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "foundry.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}
