package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/internal/engine"
)

func TestCompileAndTestHTTP(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(eng, slog.Default())

	t.Run("compile", func(t *testing.T) {
		body := bytes.NewBufferString(`{"targets":["Create2"]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/compile", body)
		w := httptest.NewRecorder()
		srv.postCompile(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		var env struct {
			Ok   bool `json:"ok"`
			Data struct {
				Artifacts []struct{ Name string } `json:"artifacts"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if !env.Ok {
			t.Fatalf("compile failed: %s", w.Body.String())
		}
		found := false
		for _, a := range env.Data.Artifacts {
			if a.Name == "Create2" {
				found = true
			}
		}
		if !found {
			t.Fatal("Create2 artifact missing")
		}
	})

	t.Run("test", func(t *testing.T) {
		body := bytes.NewBufferString(`{"match_path":"test/utils/Create2.t.sol"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/test", body)
		w := httptest.NewRecorder()
		srv.postTest(w, req)
		var env struct {
			Ok   bool `json:"ok"`
			Data struct {
				Passed int `json:"passed"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if !env.Ok || env.Data.Passed < 1 {
			t.Fatalf("test failed: %s", w.Body.String())
		}
	})
}

func TestAuthMiddleware(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(eng, slog.Default())
	srv.auth = "s3cret"
	h := srv.authMiddleware(srv.mux)

	cases := []struct {
		name   string
		method string
		path   string
		token  string
		want   int
	}{
		{"healthz public", http.MethodGet, "/healthz", "", http.StatusOK},
		{"missing token", http.MethodGet, "/v1/chains", "", http.StatusUnauthorized},
		{"wrong token", http.MethodGet, "/v1/chains", "nope", http.StatusUnauthorized},
		{"valid token", http.MethodGet, "/v1/chains", "s3cret", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, nil)
			if c.token != "" {
				req.Header.Set("Authorization", "Bearer "+c.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}
