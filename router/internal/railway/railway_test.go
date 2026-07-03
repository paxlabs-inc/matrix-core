// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package railway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/router/internal/provision"
)

// gqlServer is an httptest GraphQL endpoint that dispatches on the
// operation name embedded in the query text, mirroring the fly test
// server's per-path dispatch.
func gqlServer(t *testing.T, handle func(t *testing.T, op string, vars map[string]any) (data any, gqlErr string)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer T" {
			t.Errorf("authorization header: %q", got)
		}
		var req gqlRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		op := ""
		for _, name := range []string{"serviceCreate", "volumeCreate", "deployments", "serviceDelete", "volumeDelete"} {
			if strings.Contains(req.Query, name+"(") {
				op = name
				break
			}
		}
		data, gqlErr := handle(t, op, req.Variables)
		resp := map[string]any{}
		if data != nil {
			resp["data"] = data
		}
		if gqlErr != "" {
			resp["errors"] = []map[string]any{{"message": gqlErr}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func testProvisioner(srvURL string) *Provisioner {
	return &Provisioner{
		Client:        New("T", "proj-1", "env-1").WithEndpoint(srvURL),
		Image:         "ghcr.io/matrix/daemon:test",
		ProbeInterval: 5 * time.Millisecond,
	}
}

func TestRailwayEnsureCreatesServiceVolumeThenWaitsDeployed(t *testing.T) {
	var mu sync.Mutex
	var gotService, gotVolume bool
	deployPolls := 0
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		input, _ := vars["input"].(map[string]any)
		switch op {
		case "serviceCreate":
			gotService = true
			if input["projectId"] != "proj-1" || input["environmentId"] != "env-1" {
				t.Errorf("service scope: %+v", input)
			}
			if input["name"] != "matrix-alice-1" {
				t.Errorf("service name: %v", input["name"])
			}
			source, _ := input["source"].(map[string]any)
			if source["image"] != "ghcr.io/matrix/daemon:test" {
				t.Errorf("image: %+v", source)
			}
			env, _ := input["variables"].(map[string]any)
			if env["MATRIX_USER_ID"] != "alice-1" {
				t.Errorf("env not forwarded: %+v", env)
			}
			return map[string]any{"serviceCreate": Service{ID: "svc-1", Name: "matrix-alice-1"}}, ""
		case "volumeCreate":
			if !gotService {
				t.Errorf("volume created before service")
			}
			gotVolume = true
			if input["serviceId"] != "svc-1" || input["mountPath"] != "/data" {
				t.Errorf("volume request: %+v", input)
			}
			return map[string]any{"volumeCreate": Volume{ID: "vol-1", Name: "matrix-alice-1-volume"}}, ""
		case "deployments":
			if !gotVolume {
				t.Errorf("deployment polled before volume")
			}
			deployPolls++
			status := "BUILDING"
			if deployPolls >= 3 {
				status = DeployStatusSuccess
			}
			return map[string]any{"deployments": map[string]any{
				"edges": []map[string]any{{"node": Deployment{ID: "d-1", Status: status}}},
			}}, ""
		default:
			t.Errorf("unexpected op %q", op)
			return nil, "unexpected"
		}
	})
	defer srv.Close()

	p := testProvisioner(srv.URL)
	env, err := p.Ensure(context.Background(), provision.CreateRequest{
		UserID: "alice-1",
		Region: "us-west2",
		Env:    map[string]string{"MATRIX_USER_ID": "alice-1"},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if env.ID != "svc-1" || env.VolumeID != "vol-1" {
		t.Fatalf("env ids: %+v", env)
	}
	if !env.Ready {
		t.Fatalf("expected Ready after deployment success")
	}
	if env.Endpoint.Host != "matrix-alice-1.railway.internal" {
		t.Fatalf("endpoint host: %q", env.Endpoint.Host)
	}
	if env.Endpoint.Port != "" {
		t.Fatalf("port must be empty (router default applies): %q", env.Endpoint.Port)
	}
	if deployPolls < 3 {
		t.Fatalf("expected readiness polling, got %d polls", deployPolls)
	}
}

func TestRailwayEnsureToleratesNoDeploymentsYet(t *testing.T) {
	var mu sync.Mutex
	deployPolls := 0
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case "serviceCreate":
			return map[string]any{"serviceCreate": Service{ID: "svc-1", Name: "matrix-bob"}}, ""
		case "volumeCreate":
			return map[string]any{"volumeCreate": Volume{ID: "vol-1"}}, ""
		case "deployments":
			deployPolls++
			if deployPolls < 3 {
				// Freshly created service: empty deployment list.
				return map[string]any{"deployments": map[string]any{"edges": []map[string]any{}}}, ""
			}
			return map[string]any{"deployments": map[string]any{
				"edges": []map[string]any{{"node": Deployment{ID: "d-1", Status: DeployStatusSuccess}}},
			}}, ""
		default:
			return nil, "unexpected"
		}
	})
	defer srv.Close()
	if _, err := testProvisioner(srv.URL).Ensure(context.Background(), provision.CreateRequest{UserID: "bob"}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
}

func TestRailwayEnsureFailsOnTerminalDeployment(t *testing.T) {
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		switch op {
		case "serviceCreate":
			return map[string]any{"serviceCreate": Service{ID: "svc-1", Name: "matrix-bob"}}, ""
		case "volumeCreate":
			return map[string]any{"volumeCreate": Volume{ID: "vol-1"}}, ""
		case "deployments":
			return map[string]any{"deployments": map[string]any{
				"edges": []map[string]any{{"node": Deployment{ID: "d-1", Status: DeployStatusFailed}}},
			}}, ""
		default:
			return nil, "unexpected"
		}
	})
	defer srv.Close()
	_, err := testProvisioner(srv.URL).Ensure(context.Background(), provision.CreateRequest{UserID: "bob"})
	if err == nil || !strings.Contains(err.Error(), DeployStatusFailed) {
		t.Fatalf("want terminal-deployment error, got %v", err)
	}
}

func TestRailwayEnsureRequiresImage(t *testing.T) {
	p := &Provisioner{Client: New("T", "proj-1", "env-1")}
	if _, err := p.Ensure(context.Background(), provision.CreateRequest{UserID: "u"}); err == nil {
		t.Fatalf("expected image-not-configured error")
	}
}

func TestRailwayWakeIsNoOpWithDerivedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Wake must not call the API, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	p := testProvisioner(srv.URL)
	env, err := p.Wake(context.Background(), provision.Ref{UserID: "Alice_1", EnvID: "svc-1", VolumeID: "vol-1"})
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if env.Endpoint.Host != "matrix-alice_1.railway.internal" {
		t.Fatalf("endpoint host: %q", env.Endpoint.Host)
	}
	if !env.Ready {
		t.Fatalf("expected Ready")
	}
	if env.VolumeID != "vol-1" {
		t.Fatalf("volume id: %q", env.VolumeID)
	}
}

func TestRailwayStatusSleepingCountsAsReady(t *testing.T) {
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		if op != "deployments" {
			t.Errorf("unexpected op %q", op)
		}
		input, _ := vars["input"].(map[string]any)
		if input["serviceId"] != "svc-1" {
			t.Errorf("service id: %+v", input)
		}
		return map[string]any{"deployments": map[string]any{
			"edges": []map[string]any{{"node": Deployment{ID: "d-1", Status: DeployStatusSleeping}}},
		}}, ""
	})
	defer srv.Close()
	env, err := testProvisioner(srv.URL).Status(context.Background(), provision.Ref{UserID: "alice", EnvID: "svc-1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !env.Ready {
		t.Fatalf("SLEEPING must count as ready (wake-on-request)")
	}
	if env.State != DeployStatusSleeping {
		t.Fatalf("state: %q", env.State)
	}
}

func TestRailwayStatusMapsGraphQLNotFound(t *testing.T) {
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		return nil, "Service not found"
	})
	defer srv.Close()
	if _, err := testProvisioner(srv.URL).Status(context.Background(), provision.Ref{EnvID: "svc-gone"}); !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("want provision.ErrNotFound, got %v", err)
	}
}

func TestRailwayStatusMapsGraphQLUnauthorized(t *testing.T) {
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		return nil, "Not Authorized"
	})
	defer srv.Close()
	if _, err := testProvisioner(srv.URL).Status(context.Background(), provision.Ref{EnvID: "svc-1"}); !errors.Is(err, provision.ErrUnauthorized) {
		t.Fatalf("want provision.ErrUnauthorized, got %v", err)
	}
}

func TestRailwayStatusMapsHTTPUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := testProvisioner(srv.URL).Status(context.Background(), provision.Ref{EnvID: "svc-1"}); !errors.Is(err, provision.ErrUnauthorized) {
		t.Fatalf("want provision.ErrUnauthorized, got %v", err)
	}
}

func TestRailwayDestroyDeletesServiceThenVolume(t *testing.T) {
	var mu sync.Mutex
	var gotService, gotVolume bool
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case "serviceDelete":
			gotService = true
			if vars["id"] != "svc-1" {
				t.Errorf("service id: %+v", vars)
			}
			return map[string]any{"serviceDelete": true}, ""
		case "volumeDelete":
			if !gotService {
				t.Errorf("volume deleted before service")
			}
			gotVolume = true
			if vars["volumeId"] != "vol-1" {
				t.Errorf("volume id: %+v", vars)
			}
			return map[string]any{"volumeDelete": true}, ""
		default:
			t.Errorf("unexpected op %q", op)
			return nil, "unexpected"
		}
	})
	defer srv.Close()
	if err := testProvisioner(srv.URL).Destroy(context.Background(), provision.Ref{EnvID: "svc-1", VolumeID: "vol-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !gotVolume {
		t.Fatalf("volume not deleted")
	}
}

func TestRailwayDestroyToleratesMissingVolume(t *testing.T) {
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		switch op {
		case "serviceDelete":
			return map[string]any{"serviceDelete": true}, ""
		case "volumeDelete":
			return nil, "Volume not found"
		default:
			return nil, "unexpected"
		}
	})
	defer srv.Close()
	if err := testProvisioner(srv.URL).Destroy(context.Background(), provision.Ref{EnvID: "svc-1", VolumeID: "vol-gone"}); err != nil {
		t.Fatalf("Destroy must tolerate a vanished volume, got %v", err)
	}
}

func TestRailwayDestroyMapsServiceNotFound(t *testing.T) {
	srv := gqlServer(t, func(t *testing.T, op string, vars map[string]any) (any, string) {
		return nil, "Service not found"
	})
	defer srv.Close()
	if err := testProvisioner(srv.URL).Destroy(context.Background(), provision.Ref{EnvID: "svc-gone"}); !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("want provision.ErrNotFound, got %v", err)
	}
}

func TestRailwayWakeOnRequestIsTrue(t *testing.T) {
	if !(&Provisioner{}).WakeOnRequest() {
		t.Fatalf("railway must report wake-on-request")
	}
}

func TestRailwayServiceName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"alice-1", "matrix-alice-1"},
		{"Alice.B!ob", "matrix-alicebob"},
		{"...", "matrix-user"},
		{strings.Repeat("a", 40), "matrix-" + strings.Repeat("a", 26)},
	} {
		if got := ServiceName(tc.in); got != tc.want {
			t.Errorf("ServiceName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
