// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paxlabs-inc/machine-genome/mgs"
	"golang.org/x/sys/unix"
)

const (
	descriptorSchema = "matrix.machine-identity.v1"
	stateDirName     = "state"
	keyFileName      = "controller.key.json"
	genesisFileName  = "genesis.json"
	descriptorName   = "identity.json"
	lockFileName     = ".bootstrap.lock"
	runtimeName      = "Centra AI durable agent machine"
	runtimeVersion   = "1.0.0"
)

var (
	ErrIncompleteState    = errors.New("machine identity state is incomplete")
	ErrInsecurePermission = errors.New("machine identity path has insecure permissions")
)

type Config struct {
	Dir         string
	Name        string
	Namespace   string
	SubjectType string
	Version     string
	Description string
}

type Descriptor struct {
	Schema             string `json:"schema"`
	MGSVersion         string `json:"mgs_version"`
	Name               string `json:"name"`
	Namespace          string `json:"namespace"`
	SubjectType        string `json:"subject_type"`
	Version            string `json:"version"`
	DID                string `json:"did"`
	VerificationMethod string `json:"verification_method"`
	Gene               string `json:"gene"`
	CreatedAt          string `json:"created_at"`
}

// RuntimeConfig returns the canonical identity coordinates used by every
// process on one durable Centra AI machine. Processes must share these exact
// coordinates so concurrent startup verifies one identity instead of forking
// process-specific identities.
func RuntimeConfig(machineDataRoot string) Config {
	return Config{
		Dir:         filepath.Join(machineDataRoot, ".matrix", "machine-genome"),
		Name:        runtimeName,
		SubjectType: "agent-instance",
		Version:     runtimeVersion,
		Description: "Durable per-machine Centra AI agent runtime",
	}
}

func Ensure(ctx context.Context, cfg Config) (Descriptor, error) {
	cfg.Dir = strings.TrimSpace(cfg.Dir)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	cfg.SubjectType = strings.TrimSpace(cfg.SubjectType)
	cfg.Version = strings.TrimSpace(cfg.Version)
	cfg.Description = strings.TrimSpace(cfg.Description)
	if cfg.Dir == "" || cfg.Name == "" || cfg.Version == "" {
		return Descriptor{}, fmt.Errorf("machine identity: directory, name, and version are required")
	}
	if cfg.SubjectType == "" {
		cfg.SubjectType = "agent-instance"
	}
	if err := ctx.Err(); err != nil {
		return Descriptor{}, err
	}
	if err := ensurePrivateDirectory(cfg.Dir); err != nil {
		return Descriptor{}, err
	}
	lock, err := openLock(filepath.Join(cfg.Dir, lockFileName))
	if err != nil {
		return Descriptor{}, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: lock bootstrap: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return Descriptor{}, err
	}

	stateDir := filepath.Join(cfg.Dir, stateDirName)
	_, err = os.Lstat(stateDir)
	switch {
	case err == nil:
		return verify(ctx, cfg, stateDir)
	case !errors.Is(err, os.ErrNotExist):
		return Descriptor{}, fmt.Errorf("machine identity: inspect state: %w", err)
	}
	if err := create(ctx, cfg, stateDir); err != nil {
		return Descriptor{}, err
	}
	return verify(ctx, cfg, stateDir)
}

func create(ctx context.Context, cfg Config, stateDir string) error {
	staging, err := os.MkdirTemp(cfg.Dir, ".identity-staging-")
	if err != nil {
		return fmt.Errorf("machine identity: create staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return fmt.Errorf("machine identity: secure staging directory: %w", err)
	}
	keyPath := filepath.Join(staging, keyFileName)
	keyFile, err := mgs.GeneratePrivateKeyFile(keyPath)
	if err != nil {
		return fmt.Errorf("machine identity: generate controller: %w", err)
	}
	when := time.Now().UTC().Truncate(time.Second)
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = keyFile.DID
	}
	record := mgs.GenesisRecord{
		MGS:        mgs.Version,
		RecordType: mgs.RecordGenesis,
		Identity: mgs.Identity{
			Name: cfg.Name, Namespace: namespace, SubjectType: cfg.SubjectType,
			Version: cfg.Version, Description: cfg.Description, DID: keyFile.DID,
		},
		Parentage: []mgs.LineageEdge{},
		Controller: mgs.Controller{
			DID: keyFile.DID, GenesisKeyID: keyFile.VerificationMethod,
		},
		Genesis: mgs.GenesisMetadata{
			Timestamp: when.Format(time.RFC3339), ArtifactCommitments: []mgs.ArtifactCommitment{},
		},
	}
	unsigned, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("machine identity: encode genesis: %w", err)
	}
	unsigned = append(unsigned, '\n')
	if _, err := mgs.ValidateGenesis(unsigned, false); err != nil {
		return fmt.Errorf("machine identity: validate genesis: %w", err)
	}
	_, privateKey, err := mgs.LoadPrivateKeyFile(keyPath)
	if err != nil {
		return fmt.Errorf("machine identity: reload controller: %w", err)
	}
	secured, err := mgs.SignRecord(unsigned, mgs.NewProofOptions(when, keyFile.VerificationMethod), privateKey)
	if err != nil {
		return fmt.Errorf("machine identity: sign genesis: %w", err)
	}
	gene, err := mgs.VerifyGenesis(ctx, secured, mgs.DIDKeyResolver{})
	if err != nil {
		return fmt.Errorf("machine identity: verify new genesis: %w", err)
	}
	descriptor := Descriptor{
		Schema: descriptorSchema, MGSVersion: mgs.Version,
		Name: cfg.Name, Namespace: namespace, SubjectType: cfg.SubjectType,
		Version: cfg.Version, DID: keyFile.DID,
		VerificationMethod: keyFile.VerificationMethod, Gene: gene,
		CreatedAt: when.Format(time.RFC3339),
	}
	descriptorBytes, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return fmt.Errorf("machine identity: encode descriptor: %w", err)
	}
	descriptorBytes = append(descriptorBytes, '\n')
	if err := writeExclusive(filepath.Join(staging, genesisFileName), secured); err != nil {
		return err
	}
	if err := writeExclusive(filepath.Join(staging, descriptorName), descriptorBytes); err != nil {
		return err
	}
	if err := syncDirectory(staging); err != nil {
		return fmt.Errorf("machine identity: sync staged state: %w", err)
	}
	if err := os.Rename(staging, stateDir); err != nil {
		return fmt.Errorf("machine identity: publish state: %w", err)
	}
	if err := syncDirectory(cfg.Dir); err != nil {
		return fmt.Errorf("machine identity: sync identity directory: %w", err)
	}
	return nil
}

func verify(ctx context.Context, cfg Config, stateDir string) (Descriptor, error) {
	if err := requireMode(stateDir, true, 0o700); err != nil {
		return Descriptor{}, err
	}
	keyPath := filepath.Join(stateDir, keyFileName)
	genesisPath := filepath.Join(stateDir, genesisFileName)
	descriptorPath := filepath.Join(stateDir, descriptorName)
	for _, path := range []string{keyPath, genesisPath, descriptorPath} {
		if _, err := os.Lstat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Descriptor{}, fmt.Errorf("%w: missing %s", ErrIncompleteState, filepath.Base(path))
			}
			return Descriptor{}, fmt.Errorf("machine identity: inspect %s: %w", filepath.Base(path), err)
		}
		if err := requireMode(path, false, 0o600); err != nil {
			return Descriptor{}, err
		}
	}
	keyFile, _, err := mgs.LoadPrivateKeyFile(keyPath)
	if err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: load controller: %w", err)
	}
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: read genesis: %w", err)
	}
	gene, err := mgs.VerifyGenesis(ctx, genesisBytes, mgs.DIDKeyResolver{})
	if err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: verify genesis: %w", err)
	}
	record, err := mgs.ValidateGenesis(genesisBytes, true)
	if err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: validate secured genesis: %w", err)
	}
	descriptorBytes, err := os.ReadFile(descriptorPath)
	if err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: read descriptor: %w", err)
	}
	var descriptor Descriptor
	decoder := json.NewDecoder(strings.NewReader(string(descriptorBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("machine identity: decode descriptor: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Descriptor{}, fmt.Errorf("machine identity: descriptor has trailing JSON")
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = keyFile.DID
	}
	if keyFile.DID != record.Controller.DID || keyFile.VerificationMethod != record.Controller.GenesisKeyID ||
		record.Identity.DID != keyFile.DID {
		return Descriptor{}, fmt.Errorf("machine identity: controller, key, and subject DID do not match")
	}
	if record.Identity.Name != cfg.Name || record.Identity.Namespace != namespace ||
		record.Identity.SubjectType != cfg.SubjectType || record.Identity.Version != cfg.Version {
		return Descriptor{}, fmt.Errorf("machine identity: genesis subject does not match configured agent instance")
	}
	want := Descriptor{
		Schema: descriptorSchema, MGSVersion: mgs.Version,
		Name: record.Identity.Name, Namespace: record.Identity.Namespace,
		SubjectType: record.Identity.SubjectType, Version: record.Identity.Version,
		DID: keyFile.DID, VerificationMethod: keyFile.VerificationMethod,
		Gene: gene, CreatedAt: record.Genesis.Timestamp,
	}
	if descriptor != want {
		return Descriptor{}, fmt.Errorf("machine identity: descriptor does not match verified genesis")
	}
	return descriptor, nil
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("machine identity: create directory: %w", err)
		}
		return requireMode(path, true, 0o700)
	}
	if err != nil {
		return fmt.Errorf("machine identity: inspect directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("machine identity: path is not a private directory")
	}
	return requireMode(path, true, 0o700)
}

func openLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("machine identity: open bootstrap lock: %w", err)
	}
	if err := requireMode(path, false, 0o600); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func requireMode(path string, directory bool, want os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("machine identity: stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || directory != info.IsDir() || info.Mode().Perm() != want {
		return fmt.Errorf("%w: %s is %s, want %s", ErrInsecurePermission, path, info.Mode(), want)
	}
	return nil
}

func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("machine identity: create %s: %w", filepath.Base(path), err)
	}
	ok := false
	defer func() {
		if file != nil {
			file.Close()
		}
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("machine identity: write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("machine identity: sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("machine identity: close %s: %w", filepath.Base(path), err)
	}
	file = nil
	ok = true
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
