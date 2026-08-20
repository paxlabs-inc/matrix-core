package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"centra/gateway/internal/auth"
)

const (
	agentCoreActiveKeyIDEnv      = "MATRIX_GATEWAY_AGENTCORE_ACTIVE_KID"
	agentCoreSigningKeyEnv       = "MATRIX_GATEWAY_AGENTCORE_SIGNING_KEY"
	agentCoreVerificationKeysEnv = "MATRIX_GATEWAY_AGENTCORE_VERIFICATION_KEYS"
)

func runMintAgentCoreToken(args []string, stdout io.Writer, getenv func(string) string, now func() time.Time) error {
	fs := flag.NewFlagSet("mint-agentcore-token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	actor := fs.String("actor", "", "actor DID to bind")
	ttl := fs.Duration("ttl", auth.AgentCoreTokenTTL, "token lifetime (maximum 1h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("mint-agentcore-token: unexpected positional arguments")
	}
	keyID := strings.TrimSpace(getenv(agentCoreActiveKeyIDEnv))
	encodedKey := strings.TrimSpace(getenv(agentCoreSigningKeyEnv))
	if keyID == "" || encodedKey == "" {
		return fmt.Errorf("mint-agentcore-token: %s and %s are required", agentCoreActiveKeyIDEnv, agentCoreSigningKeyEnv)
	}
	key, err := decodeAgentCoreKey(encodedKey)
	if err != nil {
		return fmt.Errorf("mint-agentcore-token: invalid signing key: %w", err)
	}
	issuer, err := auth.NewAgentCoreIssuer(auth.AgentCoreIssuerOptions{
		KeyID: keyID,
		Key:   key,
		Now:   now,
	})
	if err != nil {
		return fmt.Errorf("mint-agentcore-token: %w", err)
	}
	token, _, err := issuer.Mint(*actor, *ttl)
	if err != nil {
		return fmt.Errorf("mint-agentcore-token: %w", err)
	}
	if _, err := fmt.Fprintln(stdout, token); err != nil {
		return fmt.Errorf("mint-agentcore-token: write token: %w", err)
	}
	return nil
}

func loadAgentCoreVerificationKeys(getenv func(string) string) (map[string][]byte, error) {
	keys := map[string][]byte{}
	add := func(keyID, encoded string) error {
		keyID = strings.TrimSpace(keyID)
		encoded = strings.TrimSpace(encoded)
		if keyID == "" || encoded == "" {
			return fmt.Errorf("AgentCore verification key id and value are required")
		}
		key, err := decodeAgentCoreKey(encoded)
		if err != nil {
			return fmt.Errorf("AgentCore verification key %q: %w", keyID, err)
		}
		if prior, exists := keys[keyID]; exists && !bytes.Equal(prior, key) {
			return fmt.Errorf("AgentCore verification key id %q has conflicting values", keyID)
		}
		keys[keyID] = key
		return nil
	}

	activeID := strings.TrimSpace(getenv(agentCoreActiveKeyIDEnv))
	activeKey := strings.TrimSpace(getenv(agentCoreSigningKeyEnv))
	if (activeID == "") != (activeKey == "") {
		return nil, fmt.Errorf("%s and %s must be configured together", agentCoreActiveKeyIDEnv, agentCoreSigningKeyEnv)
	}
	if activeID != "" {
		if err := add(activeID, activeKey); err != nil {
			return nil, err
		}
	}

	verification := strings.TrimSpace(getenv(agentCoreVerificationKeysEnv))
	if verification == "" {
		return keys, nil
	}
	for _, entry := range strings.Split(verification, ",") {
		keyID, encoded, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			return nil, fmt.Errorf("%s entries must use kid=base64url", agentCoreVerificationKeysEnv)
		}
		if err := add(keyID, encoded); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func decodeAgentCoreKey(value string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("must be unpadded base64url")
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("must decode to at least 32 bytes")
	}
	return key, nil
}
