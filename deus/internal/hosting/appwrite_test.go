package hosting_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/paxlabs-inc/deus/internal/hosting"
	"github.com/paxlabs-inc/deus/internal/objstore"
)

func TestAppwriteDeployUploadsCodeBundle(t *testing.T) {
	ctx := context.Background()

	// A real gzip stream (magic 0x1f 0x8b) standing in for the developer's
	// code.tar.gz. Deploy must pass it through unchanged as the code part.
	artifact := makeGzip(t, "developer-bundle-tar-contents")

	var mu sync.Mutex
	var (
		sawFunctionsPost bool
		variableKeys     []string
		deployContentTyp string
		fields           = map[string]string{}
		codePart         []byte
		codeFileName     string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/functions":
			sawFunctionsPost = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"$id":"fn_test"}`)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/variables"):
			var body struct {
				Key string `json:"key"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			variableKeys = append(variableKeys, body.Key)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"$id":"var_test"}`)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deployments"):
			deployContentTyp = r.Header.Get("Content-Type")
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Errorf("deployments: ParseMultipartForm: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for k, v := range r.MultipartForm.Value {
				if len(v) > 0 {
					fields[k] = v[0]
				}
			}
			fhs := r.MultipartForm.File["code"]
			if len(fhs) == 0 {
				t.Error("deployments: missing code file part")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			codeFileName = fhs[0].Filename
			f, err := fhs[0].Open()
			if err != nil {
				t.Errorf("deployments: open code part: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer f.Close()
			codePart, _ = io.ReadAll(f)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"$id":"dep_test"}`)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	mem := objstore.NewMem("test")
	key := "artifacts/svc-12345678/bundle.tar.gz"
	if err := mem.Put(ctx, key, bytes.NewReader(artifact), int64(len(artifact)), "application/gzip"); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	backend := hosting.NewAppwriteBackend(hosting.AppwriteConfig{
		Endpoint:  srv.URL,
		ProjectID: "proj",
		APIKey:    "key",
	}, mem, hosting.Limits{DefaultTimeoutMS: 30000, DefaultMaxResponseBytes: 262144})

	res, err := backend.Deploy(ctx, hosting.DeployInput{
		ServiceID:   "svc-12345678",
		ArtifactKey: key,
		Runtime:     "node20",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	if res.FunctionID != "fn_test" {
		t.Errorf("FunctionID = %q, want fn_test", res.FunctionID)
	}
	if res.DeploymentID != "dep_test" {
		t.Errorf("DeploymentID = %q, want dep_test", res.DeploymentID)
	}
	wantExec := srv.URL + "/functions/fn_test/executions"
	if res.ExecEndpoint != wantExec {
		t.Errorf("ExecEndpoint = %q, want %q", res.ExecEndpoint, wantExec)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawFunctionsPost {
		t.Error("expected POST /functions")
	}
	if !strings.HasPrefix(deployContentTyp, "multipart/form-data") {
		t.Errorf("deployment content-type = %q, want multipart/form-data", deployContentTyp)
	}
	if codeFileName != "code.tar.gz" {
		t.Errorf("code filename = %q, want code.tar.gz", codeFileName)
	}
	if len(codePart) == 0 {
		t.Fatal("code part is empty")
	}
	if !bytes.Equal(codePart, artifact) {
		t.Errorf("code part (%d bytes) does not match artifact (%d bytes)", len(codePart), len(artifact))
	}
	for _, want := range []string{"activate", "entrypoint", "commands"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("missing multipart field %q", want)
		}
	}
	if fields["activate"] != "true" {
		t.Errorf("activate = %q, want true", fields["activate"])
	}
	if fields["entrypoint"] != "src/main.js" {
		t.Errorf("entrypoint = %q, want src/main.js", fields["entrypoint"])
	}
	if !contains(variableKeys, "DEUS_MAX_RESPONSE_BYTES") {
		t.Errorf("expected DEUS_MAX_RESPONSE_BYTES variable, got %v", variableKeys)
	}
}

func TestAppwriteDeployRejectsMissingConfig(t *testing.T) {
	mem := objstore.NewMem("test")
	backend := hosting.NewAppwriteBackend(hosting.AppwriteConfig{}, mem, hosting.Limits{})
	if _, err := backend.Deploy(context.Background(), hosting.DeployInput{ServiceID: "svc", ArtifactKey: "k", Runtime: "node20"}); err == nil {
		t.Fatal("expected error for incomplete appwrite config")
	}
}

func makeGzip(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	b := buf.Bytes()
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatalf("expected gzip magic bytes, got % x", b[:2])
	}
	return b
}

func contains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
