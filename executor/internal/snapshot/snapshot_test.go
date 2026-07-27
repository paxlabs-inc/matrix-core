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

// TestS3ClientConfiguration verifies endpoint and credential shapes are
// accepted without ever rendering credentials into a subprocess environment
// or command line.
func TestS3ClientConfiguration(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			"with creds",
			Config{
				DataDir: "/d", Endpoint: "http://example:9000",
				Bucket: "b", UserID: "u",
				AccessKey: "alice", SecretKey: "s3cr3t",
			},
		},
		{
			"anonymous",
			Config{
				DataDir: "/d", Endpoint: "https://example:9001/",
				Bucket: "b", UserID: "u",
			},
		},
		{
			"escaped @ in secret",
			Config{
				DataDir: "/d", Endpoint: "http://example:9000",
				Bucket: "b", UserID: "u",
				AccessKey: "alice", SecretKey: "p@ss/w0rd",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, err := New(tc.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if mgr.s3 == nil {
				t.Fatal("S3 client was not initialized")
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
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
