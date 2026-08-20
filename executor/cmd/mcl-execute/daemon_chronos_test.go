package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	toolpolicy "centra/executor/tool"
	machinechronos "centra/packages/machine/chronos"
	"centra/packages/vault"
)

func TestLocalChronosAPIRealStoreCapabilityAndDelivery(t *testing.T) {
	wakes := make(chan map[string]interface{}, 1)
	wakeServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		wakes <- body
		response.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	directory := filepath.Join(t.TempDir(), "chronos")
	capability, err := machinechronos.EnsureCapability(directory)
	if err != nil {
		t.Fatal(err)
	}
	store, err := machinechronos.Open(context.Background(), machinechronos.Config{
		Path: filepath.Join(directory, "chronos.db"), MachineGene: "gene-api", Vault: localChronosTestVault(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := machinechronos.Start(context.Background(), machinechronos.EngineConfig{Store: store,
		Target:    machinechronos.LoopbackTarget{URL: wakeServer.URL, Capability: capability},
		RetryBase: 10 * time.Millisecond, RetryLimit: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	state := &daemonState{chronosEngine: engine, chronosStore: store, chronosCapability: capability}
	mux := http.NewServeMux()
	mux.HandleFunc("/chronos/v1/alarms", state.handleLocalChronosAlarms)
	mux.HandleFunc("/chronos/v1/alarms/", state.handleLocalChronosAlarm)
	api := httptest.NewServer(mux)
	defer api.Close()
	automatrixAlarm := map[string]interface{}{
		"kind": "cron", "cron_expr": "@every 45m", "timezone": "UTC",
		"wake_message": "run automatrix work", "idempotency_key": "automatrix-reconcile",
		"label": "Automatrix",
	}
	runChronosBridgeCall(t, api.URL, capability, "alarm_set", automatrixAlarm)
	// Neo sends the same stable alarm_set on every process boot. The local
	// decoder derives a different next_fire_at each time; that must deduplicate
	// to the durable cron alarm instead of returning an HTTP 409 and crashing
	// the co-located runtime.
	runChronosBridgeCall(t, api.URL, capability, "alarm_set", automatrixAlarm)

	unauthorized, err := http.Post(api.URL+"/chronos/v1/alarms", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.StatusCode)
	}
	fireAt := time.Now().Add(80 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	created := localChronosAPIRequest(t, capability, http.MethodPost, api.URL+"/chronos/v1/alarms", map[string]interface{}{
		"id": "api-alarm", "kind": "once", "fire_at": fireAt,
		"wake_message": "run local work", "conversation_id": "conv-local",
		"payload": map[string]interface{}{"private": "sealed"}, "idempotency_key": "api-key",
	})
	if created.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create status=%d body=%s", created.StatusCode, body)
	}
	created.Body.Close()
	listed := localChronosAPIRequest(t, capability, http.MethodGet, api.URL+"/chronos/v1/alarms", nil)
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", listed.StatusCode)
	}
	listed.Body.Close()
	runChronosBridgeCall(t, api.URL, capability, "alarm_get", map[string]interface{}{"id": "api-alarm"})
	select {
	case wake := <-wakes:
		if wake["message"] != "run local work" || wake["conversation_id"] != "conv-local" {
			t.Fatalf("wake=%v", wake)
		}
	case <-time.After(time.Second):
		t.Fatal("local alarm did not reach real loopback target")
	}
}

func TestLocalChronosAPIRescheduleAndCancelAreIdempotent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "chronos")
	capability, _ := machinechronos.EnsureCapability(directory)
	store, err := machinechronos.Open(context.Background(), machinechronos.Config{
		Path: filepath.Join(directory, "chronos.db"), MachineGene: "gene-api-mutate", Vault: localChronosTestVault(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	engine, err := machinechronos.Start(context.Background(), machinechronos.EngineConfig{Store: store,
		Target: machinechronos.LoopbackTarget{URL: target.URL, Capability: capability}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	state := &daemonState{chronosEngine: engine, chronosCapability: capability}
	mux := http.NewServeMux()
	mux.HandleFunc("/chronos/v1/alarms", state.handleLocalChronosAlarms)
	mux.HandleFunc("/chronos/v1/alarms/", state.handleLocalChronosAlarm)
	api := httptest.NewServer(mux)
	defer api.Close()
	response := localChronosAPIRequest(t, capability, http.MethodPost, api.URL+"/chronos/v1/alarms", map[string]interface{}{
		"id": "mutate", "kind": "once", "fire_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		"wake_message": "later", "idempotency_key": "mutate",
	})
	response.Body.Close()
	next := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	runChronosBridgeCall(t, api.URL, capability, "alarm_reschedule", map[string]interface{}{"id": "mutate", "next_fire_at": next})
	for range 2 {
		response = localChronosAPIRequest(t, capability, http.MethodDelete, api.URL+"/chronos/v1/alarms/mutate", nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("cancel status=%d", response.StatusCode)
		}
		response.Body.Close()
	}
}

func localChronosAPIRequest(t *testing.T, capability, method, url string, body interface{}) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+capability)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func localChronosTestVault(t *testing.T) *vault.UserVault {
	t.Helper()
	provider, err := vault.NewStaticKeyProvider(map[string][]byte{"kek1": bytes.Repeat([]byte{0x77}, 32)}, "kek1")
	if err != nil {
		t.Fatal(err)
	}
	service := vault.New(provider)
	keyfile, err := service.ProvisionUser(context.Background(), "did:matrix:local-chronos-test")
	if err != nil {
		t.Fatal(err)
	}
	userVault, err := service.OpenUser(context.Background(), keyfile)
	if err != nil {
		t.Fatal(err)
	}
	return userVault
}

func runChronosBridgeCall(t *testing.T, apiURL, capability, tool string, arguments map[string]interface{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := filepath.Join("..", "..", "..", "protocol", "tools", "chronos", "chronos.mjs")
	command := exec.CommandContext(ctx, "node", script)
	sourceEnv := append(os.Environ(),
		"MATRIX_CHRONOS_LOCAL_URL="+apiURL,
		"MATRIX_CHRONOS_LOCAL_TOKEN="+capability,
		"MATRIX_CHRONOS_URL=",
	)
	bridgeEnv, privileged, err := toolpolicy.MCPEnvironment("chronos", sourceEnv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !privileged {
		t.Fatal("chronos bridge did not retain the trusted service identity")
	}
	command.Env = bridgeEnv
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]interface{}{"name": tool, "arguments": arguments}})
	if _, err := stdin.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	_ = command.Process.Signal(os.Interrupt)
	_ = command.Wait()
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("bridge response %q: %v", line, err)
	}
	if response.Result.IsError {
		t.Fatalf("bridge returned error: %s", line)
	}
}
