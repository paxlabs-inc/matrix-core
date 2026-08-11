// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package fly

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/router/internal/provision"
)

func testProvisioner(srvURL string) *Provisioner {
	return &Provisioner{
		Client:        New("T", "matrix-daemon").WithEndpoint(srvURL),
		Image:         "registry.fly.io/matrix-daemon:test",
		VolumeSizeGB:  5,
		ProbeInterval: 5 * time.Millisecond,
	}
}

func TestFlyWakePrefersPrivateIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Machine{ID: "m-1", State: "started", PrivateIP: "fdaa:75:8960::abcd"})
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	env, err := p.Wake(context.Background(), provision.Ref{UserID: "alice", EnvID: "m-1"})
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if env.Endpoint.Host != "fdaa:75:8960::abcd" {
		t.Fatalf("endpoint host: got %q", env.Endpoint.Host)
	}
	if !env.Ready {
		t.Fatalf("expected Ready")
	}
}

func TestFlyEndpointFallsBackToInternalDNS(t *testing.T) {
	// No PrivateIP in the machine object → the endpoint falls back to
	// Fly internal DNS (<machine_id>.vm.<app>.internal).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Machine{ID: "m-abc", State: "started"})
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	env, err := p.Wake(context.Background(), provision.Ref{UserID: "alice", EnvID: "m-abc"})
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if env.Endpoint.Host != "m-abc.vm.matrix-daemon.internal" {
		t.Fatalf("expected fly internal DNS host, got %q", env.Endpoint.Host)
	}
}

func TestFlyWakeMapsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	if _, err := p.Wake(context.Background(), provision.Ref{EnvID: "m-gone"}); !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("want provision.ErrNotFound, got %v", err)
	}
}

func TestFlyWakeMapsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	if _, err := p.Wake(context.Background(), provision.Ref{EnvID: "m-1"}); !errors.Is(err, provision.ErrUnauthorized) {
		t.Fatalf("want provision.ErrUnauthorized, got %v", err)
	}
}

func TestFlyEnsureCreatesVolumeThenMachine(t *testing.T) {
	var gotVolume, gotMachine bool
	var machineReq CreateMachineRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/volumes") && r.Method == http.MethodPost:
			gotVolume = true
			var vr CreateVolumeRequest
			_ = json.NewDecoder(r.Body).Decode(&vr)
			if vr.Name != "matrix_alice_1" || vr.SizeGB != 5 {
				t.Errorf("volume request: %+v", vr)
			}
			_ = json.NewEncoder(w).Encode(Volume{ID: "vol-1", Name: vr.Name, Region: vr.Region})
		case strings.HasSuffix(r.URL.Path, "/machines") && r.Method == http.MethodPost:
			if !gotVolume {
				t.Errorf("machine created before volume")
			}
			gotMachine = true
			_ = json.NewDecoder(r.Body).Decode(&machineReq)
			_ = json.NewEncoder(w).Encode(Machine{ID: "m-new", State: "created", Region: "fra", PrivateIP: "fdaa::9"})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	env, err := p.Ensure(context.Background(), provision.CreateRequest{
		UserID: "alice-1",
		Region: "fra",
		Env:    map[string]string{"MATRIX_USER_ID": "alice-1"},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !gotMachine {
		t.Fatalf("machine not created")
	}
	if env.ID != "m-new" || env.VolumeID != "vol-1" {
		t.Fatalf("env ids: %+v", env)
	}
	if machineReq.Config.Image != "registry.fly.io/matrix-daemon:test" {
		t.Fatalf("image: %q", machineReq.Config.Image)
	}
	if len(machineReq.Config.Mounts) != 1 || machineReq.Config.Mounts[0].Path != "/data" || machineReq.Config.Mounts[0].Volume != "vol-1" {
		t.Fatalf("mounts: %+v", machineReq.Config.Mounts)
	}
	if machineReq.Config.Env["MATRIX_USER_ID"] != "alice-1" {
		t.Fatalf("env not forwarded: %+v", machineReq.Config.Env)
	}
}

func TestFlyEnsureRequiresImage(t *testing.T) {
	p := &Provisioner{Client: New("T", "matrix-daemon")}
	if _, err := p.Ensure(context.Background(), provision.CreateRequest{UserID: "u"}); err == nil {
		t.Fatalf("expected image-not-configured error")
	}
}

func TestFlyDestroyForces(t *testing.T) {
	var sawForce bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("want DELETE got %s", r.Method)
		}
		sawForce = r.URL.Query().Get("force") == "true"
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	if err := p.Destroy(context.Background(), provision.Ref{EnvID: "m-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !sawForce {
		t.Fatalf("expected force=true delete")
	}
}

func TestFlyWakeOnRequestIsFalse(t *testing.T) {
	if (&Provisioner{}).WakeOnRequest() {
		t.Fatalf("fly must not report wake-on-request")
	}
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
