package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paxlabs-inc/deus/internal/gateway"
)

func TestCallAppwriteExecutionParsesResponseBody(t *testing.T) {
	inner := gateway.HostedInvokeResponse{
		Outcome: "ok",
		Result:  map[string]any{"echo": "hi"},
		Units:   "2",
	}
	innerRaw, _ := json.Marshal(inner)

	var gotProject, gotKey, gotPath, gotMethod string
	var innerReq gateway.HostedInvokeRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProject = r.Header.Get("X-Appwrite-Project")
		gotKey = r.Header.Get("X-Appwrite-Key")
		var execReq struct {
			Body   string `json:"body"`
			Path   string `json:"path"`
			Method string `json:"method"`
			Async  bool   `json:"async"`
		}
		if err := json.NewDecoder(r.Body).Decode(&execReq); err != nil {
			t.Errorf("decode exec request: %v", err)
		}
		gotPath = execReq.Path
		gotMethod = execReq.Method
		if execReq.Async {
			t.Error("expected async=false")
		}
		_ = json.Unmarshal([]byte(execReq.Body), &innerReq)

		resp := map[string]any{
			"responseStatusCode": 200,
			"responseBody":       string(innerRaw),
			"status":             "completed",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	req := gateway.HostedInvokeRequest{
		InvocationID:  "inv-1",
		Operation:     "echo",
		Args:          map[string]any{"message": "hi"},
		CallerDID:     "did:test",
		DeadlineMS:    5000,
		ReceiptDigest: "0xdeadbeef",
	}
	res, err := gateway.CallAppwriteExecution(context.Background(), srv.URL, "proj", "secret", req, 5000, 262144)
	if err != nil {
		t.Fatalf("CallAppwriteExecution: %v", err)
	}
	if res.Outcome != "ok" {
		t.Errorf("Outcome = %q, want ok", res.Outcome)
	}
	if res.Result["echo"] != "hi" {
		t.Errorf("Result = %+v, want echo=hi", res.Result)
	}
	if res.Units != "2" {
		t.Errorf("Units = %q, want 2", res.Units)
	}
	if gotProject != "proj" || gotKey != "secret" {
		t.Errorf("auth headers = %q/%q, want proj/secret", gotProject, gotKey)
	}
	if gotPath != "/invoke" || gotMethod != "POST" {
		t.Errorf("exec routing = %q %q, want POST /invoke", gotMethod, gotPath)
	}
	if innerReq.Operation != "echo" || innerReq.ReceiptDigest != "0xdeadbeef" {
		t.Errorf("inner request not forwarded verbatim: %+v", innerReq)
	}
}

func TestCallAppwriteExecutionEnforcesMaxBytes(t *testing.T) {
	big := strings.Repeat("x", 4096)
	inner, _ := json.Marshal(gateway.HostedInvokeResponse{
		Outcome: "ok",
		Result:  map[string]any{"blob": big},
		Units:   "1",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"responseStatusCode": 200,
			"responseBody":       string(inner),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	_, err := gateway.CallAppwriteExecution(context.Background(), srv.URL, "proj", "key",
		gateway.HostedInvokeRequest{InvocationID: "inv-2", Operation: "echo"}, 5000, 256)
	if err == nil {
		t.Fatal("expected max-bytes error for oversized response body")
	}
	if !strings.Contains(err.Error(), "max bytes") {
		t.Errorf("error = %v, want max-bytes failure", err)
	}
}

func TestCallAppwriteExecutionPropagatesFunctionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"responseStatusCode": 500,
			"responseBody":       `{"outcome":"error"}`,
			"status":             "completed",
			"errors":             "boom",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	_, err := gateway.CallAppwriteExecution(context.Background(), srv.URL, "proj", "key",
		gateway.HostedInvokeRequest{InvocationID: "inv-3", Operation: "echo"}, 5000, 262144)
	if err == nil {
		t.Fatal("expected error when function responseStatusCode is 5xx")
	}
}

func TestIsAppwriteExecutionsURL(t *testing.T) {
	cases := map[string]bool{
		"https://cloud.paxeer.app/v1/functions/abc/executions":  true,
		"https://cloud.paxeer.app/v1/functions/abc/executions/": true,
		"http://127.0.0.1:18080":                                false,
		"http://127.0.0.1:18080/invoke":                         false,
	}
	for url, want := range cases {
		if got := gateway.ExportIsAppwriteExecutionsURL(url); got != want {
			t.Errorf("isAppwriteExecutionsURL(%q) = %v, want %v", url, got, want)
		}
	}
}
