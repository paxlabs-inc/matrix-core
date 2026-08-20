// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const capabilityFile = "capability"

func EnsureCapability(directory string) (string, error) {
	if err := ensurePrivateDirectory(directory); err != nil {
		return "", err
	}
	path := filepath.Join(directory, capabilityFile)
	data, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return "", fmt.Errorf("local chronos: capability must be a regular mode 0600 file")
		}
		value := strings.TrimSpace(string(data))
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		if decodeErr != nil || len(decoded) != 32 {
			return "", fmt.Errorf("local chronos: capability file is corrupt")
		}
		zero(decoded)
		return value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("local chronos: read capability: %w", err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("local chronos: generate capability: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw[:])
	zero(raw[:])
	temporary, err := os.CreateTemp(directory, ".capability-")
	if err != nil {
		return "", err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.WriteString(value + "\n"); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Link(temporaryName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return EnsureCapability(directory)
		}
		return "", fmt.Errorf("local chronos: publish capability: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	if err := directoryHandle.Sync(); err != nil {
		directoryHandle.Close()
		return "", err
	}
	if err := directoryHandle.Close(); err != nil {
		return "", err
	}
	return value, nil
}
