// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package fly

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetMachineDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/machines/m-1") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer T" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Machine{ID: "m-1", State: "started", Region: "fra", PrivateIP: "fdaa::1"})
	}))
	defer srv.Close()

	c := New("T", "matrix-daemon").WithEndpoint(srv.URL)
	m, err := c.GetMachine(context.Background(), "m-1")
	if err != nil {
		t.Fatalf("GetMachine: %v", err)
	}
	if !m.Started() || m.PrivateIP != "fdaa::1" {
		t.Fatalf("unexpected machine: %+v", m)
	}
}

func TestStartMachineNoBody(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("want POST got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/start") {
			t.Errorf("want /start, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New("T", "matrix-daemon").WithEndpoint(srv.URL)
	if err := c.StartMachine(context.Background(), "m-1"); err != nil {
		t.Fatalf("StartMachine: %v", err)
	}
	if called.Load() != 1 {
		t.Fatalf("call count: got %d", called.Load())
	}
}

func TestEnsureStartedAlreadyRunning(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(Machine{ID: "m-1", State: "started"})
	}))
	defer srv.Close()
	c := New("T", "matrix-daemon").WithEndpoint(srv.URL)
	m, err := c.EnsureStarted(context.Background(), "m-1", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if !m.Started() {
		t.Fatalf("not started")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected single GET, got %d", calls.Load())
	}
}

func TestEnsureStartedWaitsForBoot(t *testing.T) {
	var (
		gets       atomic.Int32
		startCount atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			startCount.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			n := gets.Add(1)
			state := "stopped"
			if n >= 3 {
				state = "started"
			}
			_ = json.NewEncoder(w).Encode(Machine{ID: "m-1", State: state})
		}
	}))
	defer srv.Close()
	c := New("T", "matrix-daemon").WithEndpoint(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m, err := c.EnsureStarted(ctx, "m-1", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if !m.Started() {
		t.Fatalf("not started")
	}
	if startCount.Load() != 1 {
		t.Fatalf("StartMachine should fire exactly once, got %d", startCount.Load())
	}
}

func TestUnauthorizedMaps401To404Sentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New("bad", "x").WithEndpoint(srv.URL)
	_, err := c.GetMachine(context.Background(), "m-1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestNotFoundMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New("T", "matrix-daemon").WithEndpoint(srv.URL)
	_, err := c.GetMachine(context.Background(), "m-1")
	if !errors.Is(err, ErrMachineNotFound) {
		t.Fatalf("want ErrMachineNotFound, got %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
