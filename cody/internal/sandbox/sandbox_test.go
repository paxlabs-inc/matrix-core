// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordedReq captures what the client actually sent, so tests assert the
// request (auth header, GraphQL query, variables) as well as the parse. This
// exercises the real HTTP/GraphQL/marshal/parse code path end-to-end against a
// real local server — it is not a stub of the client.
type recordedReq struct {
	auth      string
	accept    string
	contentTy string
	body      gqlBody
	rawBody   string
}

type gqlBody struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// fixtureServer stands up an httptest.Server that routes on the GraphQL
// operation name embedded in the query and replies with realistic Railway
// GraphQL JSON. It records the last request for assertion.
func fixtureServer(t *testing.T, respByOp map[string]string) (*httptest.Server, *recordedReq) {
	t.Helper()
	rec := &recordedReq{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.auth = r.Header.Get("Authorization")
		rec.accept = r.Header.Get("Accept")
		rec.contentTy = r.Header.Get("Content-Type")
		rec.rawBody = string(raw)
		_ = json.Unmarshal(raw, &rec.body)

		op := opName(rec.body.Query)
		resp, ok := respByOp[op]
		if !ok {
			t.Errorf("fixture server got unexpected op %q; query=%s", op, rec.body.Query)
			http.Error(w, "unexpected op", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// opName extracts the mutation/query name (e.g. "SandboxCreate") from a query.
func opName(q string) string {
	for _, kw := range []string{"mutation ", "query "} {
		if i := strings.Index(q, kw); i >= 0 {
			rest := q[i+len(kw):]
			if j := strings.IndexAny(rest, "(  {\n"); j >= 0 {
				return rest[:j]
			}
		}
	}
	return ""
}

func newTestClient(endpoint string) Client {
	return New(Config{
		Token:         "test-token-abc",
		ProjectID:     "proj-1",
		EnvironmentID: "env-1",
		Endpoint:      endpoint,
		Timeout:       5 * time.Second,
	})
}

func TestCreate(t *testing.T) {
	tests := []struct {
		name       string
		opts       CreateOpts
		resp       string
		wantHost   string
		wantStatus string
		wantNet    string // networkMode the client must have sent
		wantEnv    bool
		wantErr    error
	}{
		{
			name:       "private default with env",
			opts:       CreateOpts{Image: "railway/sandbox:node22", Env: map[string]string{"NODE_ENV": "production"}},
			resp:       `{"data":{"sandboxCreate":{"id":"sbx_123","privateHost":"sbx-123.railway.internal","status":"CREATING"}}}`,
			wantHost:   "sbx-123.railway.internal",
			wantStatus: StatusCreating,
			wantNet:    "PRIVATE",
			wantEnv:    true,
		},
		{
			name:       "explicit isolated no env",
			opts:       CreateOpts{Image: "railway/sandbox:base", Network: NetworkIsolated},
			resp:       `{"data":{"sandboxCreate":{"id":"sbx_9","privateHost":"sbx-9.railway.internal","status":"RUNNING"}}}`,
			wantHost:   "sbx-9.railway.internal",
			wantStatus: StatusRunning,
			wantNet:    "ISOLATED",
			wantEnv:    false,
		},
		{
			name:    "graphql not-authorized error",
			opts:    CreateOpts{Image: "x"},
			resp:    `{"errors":[{"message":"Not Authorized to access project"}]}`,
			wantErr: ErrUnauthorized,
		},
		{
			name:    "graphql generic upstream error",
			opts:    CreateOpts{Image: "x"},
			resp:    `{"errors":[{"message":"internal server explosion"}]}`,
			wantErr: ErrUpstream,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := fixtureServer(t, map[string]string{"SandboxCreate": tt.resp})
			c := newTestClient(srv.URL)

			sb, err := c.Create(context.Background(), tt.opts)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if sb.PrivateHost != tt.wantHost {
				t.Errorf("PrivateHost = %q, want %q", sb.PrivateHost, tt.wantHost)
			}
			if sb.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", sb.Status, tt.wantStatus)
			}
			// Assert the request the client actually sent.
			if rec.auth != "Bearer test-token-abc" {
				t.Errorf("auth header = %q, want Bearer test-token-abc", rec.auth)
			}
			if !strings.HasPrefix(rec.contentTy, "application/json") {
				t.Errorf("content-type = %q", rec.contentTy)
			}
			input, _ := rec.body.Variables["input"].(map[string]any)
			if input == nil {
				t.Fatalf("input variable missing; vars=%v", rec.body.Variables)
			}
			if got := input["networkMode"]; got != tt.wantNet {
				t.Errorf("networkMode sent = %v, want %v", got, tt.wantNet)
			}
			if got := input["projectId"]; got != "proj-1" {
				t.Errorf("projectId sent = %v, want proj-1", got)
			}
			if got := input["environmentId"]; got != "env-1" {
				t.Errorf("environmentId sent = %v, want env-1", got)
			}
			_, hasEnv := input["variables"]
			if hasEnv != tt.wantEnv {
				t.Errorf("variables present = %v, want %v", hasEnv, tt.wantEnv)
			}
		})
	}
}

func TestExec(t *testing.T) {
	tests := []struct {
		name     string
		cmd      []string
		resp     string
		wantOut  string
		wantErrS string
		wantExit int
		wantDur  time.Duration
		wantErr  error
	}{
		{
			name:     "success exit 0",
			cmd:      []string{"sh", "-c", "echo hi"},
			resp:     `{"data":{"sandboxExec":{"stdout":"hi\n","stderr":"","exitCode":0,"durationMs":1500}}}`,
			wantOut:  "hi\n",
			wantExit: 0,
			wantDur:  1500 * time.Millisecond,
		},
		{
			name:     "nonzero exit surfaced not error",
			cmd:      []string{"false"},
			resp:     `{"data":{"sandboxExec":{"stdout":"","stderr":"boom","exitCode":1,"durationMs":42}}}`,
			wantErrS: "boom",
			wantExit: 1,
			wantDur:  42 * time.Millisecond,
		},
		{
			name:    "sandbox not found",
			cmd:     []string{"ls"},
			resp:    `{"errors":[{"message":"Sandbox does not exist"}]}`,
			wantErr: ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := fixtureServer(t, map[string]string{"SandboxExec": tt.resp})
			c := newTestClient(srv.URL)

			res, err := c.Exec(context.Background(), "sbx_123", tt.cmd)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if res.Stdout != tt.wantOut {
				t.Errorf("Stdout = %q, want %q", res.Stdout, tt.wantOut)
			}
			if res.Stderr != tt.wantErrS {
				t.Errorf("Stderr = %q, want %q", res.Stderr, tt.wantErrS)
			}
			if res.Exit != tt.wantExit {
				t.Errorf("Exit = %d, want %d", res.Exit, tt.wantExit)
			}
			if res.Duration != tt.wantDur {
				t.Errorf("Duration = %v, want %v", res.Duration, tt.wantDur)
			}
			// Assert argv was sent as a JSON array.
			input, _ := rec.body.Variables["input"].(map[string]any)
			cmdSent, ok := input["command"].([]any)
			if !ok || len(cmdSent) != len(tt.cmd) {
				t.Fatalf("command sent = %v, want %v", input["command"], tt.cmd)
			}
			for i, part := range tt.cmd {
				if cmdSent[i] != part {
					t.Errorf("command[%d] = %v, want %v", i, cmdSent[i], part)
				}
			}
			if input["sandboxId"] != "sbx_123" {
				t.Errorf("sandboxId sent = %v", input["sandboxId"])
			}
		})
	}
}

func TestWriteFiles(t *testing.T) {
	t.Run("writes files with correct payload", func(t *testing.T) {
		srv, rec := fixtureServer(t, map[string]string{
			"SandboxFilesWrite": `{"data":{"sandboxFilesWrite":{"written":2}}}`,
		})
		c := newTestClient(srv.URL)
		files := []File{
			{Path: "package.json", Content: `{"name":"app"}`},
			{Path: "src/index.ts", Content: "console.log(1)"},
		}
		if err := c.WriteFiles(context.Background(), "sbx_123", files); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		input, _ := rec.body.Variables["input"].(map[string]any)
		sent, ok := input["files"].([]any)
		if !ok || len(sent) != 2 {
			t.Fatalf("files sent = %v, want 2", input["files"])
		}
		first, _ := sent[0].(map[string]any)
		if first["path"] != "package.json" || first["content"] != `{"name":"app"}` {
			t.Errorf("first file = %v", first)
		}
	})

	t.Run("empty files is a no-op with no request", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		t.Cleanup(srv.Close)
		c := newTestClient(srv.URL)
		if err := c.WriteFiles(context.Background(), "sbx_123", nil); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if called {
			t.Error("empty WriteFiles must not hit the server")
		}
	})

	t.Run("upstream error surfaced", func(t *testing.T) {
		srv, _ := fixtureServer(t, map[string]string{
			"SandboxFilesWrite": `{"errors":[{"message":"disk full"}]}`,
		})
		c := newTestClient(srv.URL)
		err := c.WriteFiles(context.Background(), "sbx_123", []File{{Path: "a", Content: "b"}})
		if !errors.Is(err, ErrUpstream) {
			t.Fatalf("err = %v, want ErrUpstream", err)
		}
	})
}

func TestDestroy(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv, rec := fixtureServer(t, map[string]string{
			"SandboxDestroy": `{"data":{"sandboxDestroy":true}}`,
		})
		c := newTestClient(srv.URL)
		if err := c.Destroy(context.Background(), "sbx_123"); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if rec.body.Variables["id"] != "sbx_123" {
			t.Errorf("id sent = %v", rec.body.Variables["id"])
		}
	})

	t.Run("already-gone is idempotent success", func(t *testing.T) {
		srv, _ := fixtureServer(t, map[string]string{
			"SandboxDestroy": `{"errors":[{"message":"Sandbox not found"}]}`,
		})
		c := newTestClient(srv.URL)
		if err := c.Destroy(context.Background(), "sbx_gone"); err != nil {
			t.Fatalf("Destroy of gone sandbox should be nil, got %v", err)
		}
	})

	t.Run("real upstream error propagates", func(t *testing.T) {
		srv, _ := fixtureServer(t, map[string]string{
			"SandboxDestroy": `{"errors":[{"message":"rate limited"}]}`,
		})
		c := newTestClient(srv.URL)
		err := c.Destroy(context.Background(), "sbx_123")
		if !errors.Is(err, ErrUpstream) {
			t.Fatalf("err = %v, want ErrUpstream", err)
		}
	})
}

// TestHTTPStatusErrors covers the HTTP-status branch (Railway edge/auth returns
// real HTTP 401/403/5xx, distinct from the 200-with-errors-array path).
func TestHTTPStatusErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"401 unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"403 forbidden", http.StatusForbidden, ErrUnauthorized},
		{"500 upstream", http.StatusInternalServerError, ErrUpstream},
		{"429 upstream", http.StatusTooManyRequests, ErrUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", tt.status)
			}))
			t.Cleanup(srv.Close)
			c := newTestClient(srv.URL)
			_, err := c.Create(context.Background(), CreateOpts{Image: "x"})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

// TestContextCancellation proves a cancelled context aborts an in-flight call.
func TestContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test lets go
		_, _ = io.WriteString(w, `{"data":{"sandboxCreate":{"id":"x"}}}`)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := newTestClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := c.Create(ctx, CreateOpts{Image: "x"})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestNewDefaults proves the constructor's zero-value fallbacks.
func TestNewDefaults(t *testing.T) {
	c := New(Config{Token: "t"}).(*client)
	if c.endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want default %q", c.endpoint, DefaultEndpoint)
	}
	if c.hc == nil || c.hc.Timeout != defaultTimeout {
		t.Errorf("hc timeout = %v, want %v", c.hc.Timeout, defaultTimeout)
	}
}

// TestErrorResponseBodyTruncation makes sure a large HTTP error body does not
// blow up and gets truncated into the error message.
func TestErrorResponseBodyTruncation(t *testing.T) {
	big := strings.Repeat("A", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, big)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(srv.URL)
	_, err := c.Create(context.Background(), CreateOpts{Image: "x"})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("expected truncation marker in %q", err.Error())
	}
}
