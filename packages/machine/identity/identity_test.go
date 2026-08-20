// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureCreatesAndReusesVerifiedIdentity(t *testing.T) {
	cfg := testConfig(t, "neo")
	first, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(cfg.Dir, stateDirName)
	before := readState(t, state)
	second, err := Ensure(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	after := readState(t, state)
	if first != second {
		t.Fatalf("identity changed across boots:\nfirst=%+v\nsecond=%+v", first, second)
	}
	for name, value := range before {
		if string(value) != string(after[name]) {
			t.Fatalf("%s changed across boots", name)
		}
	}
	if first.Gene == "" || first.DID == "" || first.VerificationMethod == "" {
		t.Fatalf("bootstrap returned incomplete descriptor: %+v", first)
	}
	assertMode(t, cfg.Dir, 0o700)
	assertMode(t, state, 0o700)
	assertMode(t, filepath.Join(cfg.Dir, lockFileName), 0o600)
	for _, name := range []string{keyFileName, genesisFileName, descriptorName} {
		assertMode(t, filepath.Join(state, name), 0o600)
	}
}

func TestEnsureSerializesConcurrentFirstBoot(t *testing.T) {
	cfg := testConfig(t, "workforce")
	const callers = 12
	var wg sync.WaitGroup
	results := make(chan Descriptor, callers)
	errorsSeen := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Ensure(context.Background(), cfg)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	var want Descriptor
	count := 0
	for got := range results {
		if count == 0 {
			want = got
		} else if got != want {
			t.Fatalf("concurrent bootstrap forked identity:\nwant=%+v\ngot=%+v", want, got)
		}
		count++
	}
	if count != callers {
		t.Fatalf("got %d successful callers, want %d", count, callers)
	}
}

func TestEnsureFailsClosedOnPartialState(t *testing.T) {
	cfg := testConfig(t, "partial")
	if _, err := Ensure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.Dir, stateDirName, descriptorName)); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(context.Background(), cfg)
	if !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("partial state error = %v, want ErrIncompleteState", err)
	}
}

func TestEnsureFailsClosedOnCorruptGenesis(t *testing.T) {
	cfg := testConfig(t, "corrupt")
	if _, err := Ensure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Dir, stateDirName, genesisFileName)
	if err := os.WriteFile(path, []byte("{\"recordType\":\"genesis\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), cfg); err == nil {
		t.Fatal("corrupt genesis was accepted or regenerated")
	}
}

func TestEnsureFailsClosedOnMismatchedController(t *testing.T) {
	first := testConfig(t, "first")
	second := testConfig(t, "second")
	if _, err := Ensure(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.ReadFile(filepath.Join(second.Dir, stateDirName, keyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Dir, stateDirName, keyFileName), replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), first); err == nil {
		t.Fatal("mismatched controller was accepted or regenerated")
	}
}

func TestEnsureRejectsWeakPermissions(t *testing.T) {
	cfg := testConfig(t, "permissions")
	if _, err := Ensure(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Dir, stateDirName, keyFileName)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(context.Background(), cfg)
	if !errors.Is(err, ErrInsecurePermission) {
		t.Fatalf("permission error = %v, want ErrInsecurePermission", err)
	}
}

func testConfig(t *testing.T, name string) Config {
	t.Helper()
	return Config{
		Dir: filepath.Join(t.TempDir(), "machine-genome"), Name: name,
		SubjectType: "agent-instance", Version: "1.0.0",
		Description: "Centra AI durable agent instance",
	}
}

func readState(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	for _, name := range []string{keyFileName, genesisFileName, descriptorName} {
		value, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		result[name] = value
	}
	return result
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %s, want %s", path, info.Mode().Perm(), want)
	}
}
