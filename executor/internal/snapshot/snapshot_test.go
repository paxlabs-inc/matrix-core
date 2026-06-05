// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewIncomplete asserts the missing-field error path. Each missing
// required field should be surfaced in the error message so operators
// see exactly which env / flag is unset.
func TestNewIncomplete(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{"all empty", Config{}, "DataDir"},
		{"only data-dir", Config{DataDir: "/data"}, "Endpoint"},
		{"only endpoint", Config{Endpoint: "http://x:9000"}, "DataDir"},
		{"missing user-id", Config{
			DataDir:  "/data",
			Endpoint: "http://x:9000",
			Bucket:   "matrix-state",
		}, "UserID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if !errors.Is(err, ErrIncomplete) {
				t.Fatalf("want ErrIncomplete, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error to mention %q, got %q", tc.wantSub, err.Error())
			}
		})
	}
}

// TestNewMalformedEndpoint covers the URL-parse path: scheme/host
// must be present.
func TestNewMalformedEndpoint(t *testing.T) {
	cases := []struct {
		name string
		ep   string
	}{
		{"no scheme", "box.matrix.wg:9000"},
		{"empty host", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				DataDir:  "/data",
				Endpoint: tc.ep,
				Bucket:   "matrix-state",
				UserID:   "alice",
			})
			if err == nil {
				t.Fatalf("expected error for %q", tc.ep)
			}
		})
	}
}

// TestSeededSentinelLifecycle covers the on-disk contract: IsSeeded is
// false on a fresh data dir, true after markSeeded, and the sentinel
// path is .matrix/seeded relative to DataDir.
func TestSeededSentinelLifecycle(t *testing.T) {
	dir := t.TempDir()
	mgr, err := New(Config{
		DataDir:  dir,
		Endpoint: "http://example.invalid:9000",
		Bucket:   "b",
		UserID:   "u",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mgr.IsSeeded() {
		t.Fatalf("fresh dir should not be seeded")
	}
	if got, want := mgr.SeededPath(), filepath.Join(dir, SeededSentinel); got != want {
		t.Fatalf("SeededPath: got %q want %q", got, want)
	}
	if err := mgr.markSeeded(); err != nil {
		t.Fatalf("markSeeded: %v", err)
	}
	if !mgr.IsSeeded() {
		t.Fatalf("after markSeeded, IsSeeded should be true")
	}
	// Sentinel parent dir was created.
	if st, err := os.Stat(filepath.Join(dir, ".matrix")); err != nil || !st.IsDir() {
		t.Fatalf("expected .matrix dir, got %v %v", st, err)
	}
}

// TestMCEnvComposition verifies the MC_HOST_<alias> wire format used
// to pass credentials to the mc subprocess. URL-special chars in
// secrets must be escaped so they don't break the URL parser.
func TestMCEnvComposition(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		wantEnv  string
		excludes []string
	}{
		{
			"with creds",
			Config{
				DataDir: "/d", Endpoint: "http://example:9000",
				Bucket: "b", UserID: "u",
				AccessKey: "alice", SecretKey: "s3cr3t",
			},
			"http://alice:s3cr3t@example:9000",
			nil,
		},
		{
			"anonymous",
			Config{
				DataDir: "/d", Endpoint: "https://example:9001/",
				Bucket: "b", UserID: "u",
			},
			// Trailing root "/" is intentionally stripped by New so
			// the env var is the canonical scheme://host[:port] form
			// mc accepts; no creds were supplied so no '@'.
			"https://example:9001",
			[]string{"@"},
		},
		{
			"escaped @ in secret",
			Config{
				DataDir: "/d", Endpoint: "http://example:9000",
				Bucket: "b", UserID: "u",
				AccessKey: "alice", SecretKey: "p@ss/w0rd",
			},
			// url.QueryEscape: '@' -> %40 ; '/' -> %2F
			"http://alice:p%40ss%2Fw0rd@example:9000",
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, err := New(tc.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if mgr.mcEnv != tc.wantEnv {
				t.Fatalf("mcEnv: got %q want %q", mgr.mcEnv, tc.wantEnv)
			}
			for _, ex := range tc.excludes {
				if strings.Contains(mgr.mcEnv, ex) {
					t.Fatalf("mcEnv should not contain %q: %q", ex, mgr.mcEnv)
				}
			}
		})
	}
}

// TestRemotePathLayout pins the object-key layout per matrix.kvx S25Q6.
func TestRemotePathLayout(t *testing.T) {
	mgr, err := New(Config{
		DataDir:  "/d",
		Endpoint: "http://example:9000",
		Bucket:   "matrix-state",
		UserID:   "supabase|abc123",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gotPrefix := mgr.userPrefix()
	if gotPrefix != "users/supabase|abc123" {
		t.Fatalf("userPrefix: got %q", gotPrefix)
	}
	got := mgr.remotePath(gotPrefix + "/latest.tar.zst")
	want := mcAlias + "/matrix-state/users/supabase|abc123/latest.tar.zst"
	if got != want {
		t.Fatalf("remotePath: got %q want %q", got, want)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
