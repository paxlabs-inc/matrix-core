package server

import (
	"bufio"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// mjsBridge drives the REAL tools/deus/deus.mjs MCP stdio bridge as a
// subprocess — the exact binary agents run — against the real rig.
type mjsBridge struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	nextID int64
}

func startMJSBridge(t *testing.T, rig *lxpTestRig, keyfile, journal, maxSpend, maxDaily string) *mjsBridge {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping deus.mjs handshake test")
	}
	script := filepath.Join("..", "..", "..", "tools", "deus", "deus.mjs")
	cmd := exec.Command(nodeBin, script)
	cmd.Env = append(os.Environ(),
		"MATRIX_DEUS_URL="+rig.ts.URL,
		"PAXEER_AGENT_AUTH_DISABLE=1",
		"PAXEER_AGENT_KEYFILE="+keyfile,
		"PAXEER_AGENT_LABEL=lxpjs",
		"LAYERX_MAX_SPEND_USDX="+maxSpend,
		"LAYERX_MAX_DAILY_USDX="+maxDaily,
		"LAYERX_SPEND_JOURNAL="+journal,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start deus.mjs: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	b := &mjsBridge{t: t, cmd: cmd, stdin: stdin, lines: sc}
	b.call("initialize", map[string]any{})
	return b
}

func (b *mjsBridge) call(method string, params map[string]any) map[string]any {
	b.t.Helper()
	b.nextID++
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": b.nextID, "method": method, "params": params})
	if _, err := b.stdin.Write(append(req, '\n')); err != nil {
		b.t.Fatalf("bridge write: %v", err)
	}
	deadline := time.After(30 * time.Second)
	for {
		lineCh := make(chan bool, 1)
		go func() { lineCh <- b.lines.Scan() }()
		select {
		case ok := <-lineCh:
			if !ok {
				b.t.Fatalf("bridge closed stdout (scan err: %v)", b.lines.Err())
			}
		case <-deadline:
			b.t.Fatalf("bridge response timeout for %s", method)
		}
		var resp struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if err := json.Unmarshal(b.lines.Bytes(), &resp); err != nil {
			continue
		}
		if resp.ID != b.nextID {
			continue
		}
		if resp.Error != nil {
			b.t.Fatalf("bridge rpc error: %v", resp.Error)
		}
		return resp.Result
	}
}

// invokeTool calls deus_invoke through the bridge and decodes the MCP tool
// result envelope {ok, data|error}.
func (b *mjsBridge) invokeTool(rig *lxpTestRig, idem string) (map[string]any, bool) {
	b.t.Helper()
	res := b.call("tools/call", map[string]any{
		"name": "deus_invoke",
		"arguments": map[string]any{
			"service_id":      rig.serviceID,
			"operation":       "forecast",
			"args":            map[string]any{"city": "berlin"},
			"idempotency_key": idem,
		},
	})
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		b.t.Fatalf("tool result carried no content: %v", res)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var envelope map[string]any
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		b.t.Fatalf("tool result not json: %q", text)
	}
	ok, _ := envelope["ok"].(bool)
	return envelope, ok
}

// TestDeusMJSHandshake proves req.9: the real deus.mjs bridge receives the
// lxp 402 challenge, enforces the daemon-side leash BEFORE signing, signs the
// canonical LayerX intent with the executor key, retries once, and surfaces
// the LayerX receipt in the tool result — while over-leash charges surface
// the terms unsigned and cost nothing.
func TestDeusMJSHandshake(t *testing.T) {
	rig, ctx := newLXPRig(t, lxpRigOpts{})

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pubHex := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	agentDID := "did:matrix:lxpjs:" + pubHex[:16]

	dir := t.TempDir()
	keyfile := filepath.Join(dir, "executor.key")
	if err := os.WriteFile(keyfile, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "lxp-spend.json")

	if err := rig.harness.CreditDeposit(ctx, agentDID, "0xabc", "0xdep-"+agentDID, 1_000_000); err != nil {
		t.Fatalf("fund agent: %v", err)
	}

	// 1. Within leash: the bridge auto-pays and surfaces the receipt.
	bridge := startMJSBridge(t, rig, keyfile, journal, "0.050000", "0.100000")
	envelope, ok := bridge.invokeTool(rig, fmt.Sprintf("mjs-%d", time.Now().UnixNano()))
	if !ok {
		t.Fatalf("bridge invoke failed: %v", envelope)
	}
	data, _ := envelope["data"].(map[string]any)
	if data["outcome"] != "ok" {
		t.Fatalf("outcome = %v", data["outcome"])
	}
	receipt, _ := data["layerx_receipt"].(map[string]any)
	if receipt == nil || receipt["seq"] == nil || receipt["amount_usdx"] != "0.031500" {
		t.Fatalf("tool result carried no layerx receipt: %v", data)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, rig.payeeDID); bal != 31_500 {
		t.Fatalf("payee balance = %d, want 31500", bal)
	}
	if n := atomic.LoadInt64(rig.executions); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	// The spend landed in the daemon-side journal.
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("spend journal missing: %v", err)
	}
	var j struct {
		Entries []struct {
			Micro int64 `json:"micro"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &j); err != nil || len(j.Entries) != 1 || j.Entries[0].Micro != 31_500 {
		t.Fatalf("bad spend journal: %s (%v)", raw, err)
	}

	// 2. Over the per-call leash: terms surface, nothing is signed or charged.
	tight := startMJSBridge(t, rig, keyfile, journal, "0.010000", "0.100000")
	envelope, ok = tight.invokeTool(rig, fmt.Sprintf("mjs-tight-%d", time.Now().UnixNano()))
	if ok {
		t.Fatalf("over-leash invoke succeeded: %v", envelope)
	}
	errText, _ := envelope["error"].(string)
	detail, _ := envelope["detail"].(map[string]any)
	if detail == nil || detail["reason"] != "over_per_call_leash" || detail["terms"] == nil {
		t.Fatalf("over-leash surface missing reason/terms: %v (err %q)", envelope, errText)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, rig.payeeDID); bal != 31_500 {
		t.Fatalf("payee balance moved on refused payment: %d", bal)
	}
	if n := atomic.LoadInt64(rig.executions); n != 1 {
		t.Fatalf("executions after refused payment = %d, want 1", n)
	}

	// 3. Rolling daily leash across bridge restarts (shared journal): 0.0315
	//    already spent today, a 0.05 daily cap refuses the next 0.0315.
	daily := startMJSBridge(t, rig, keyfile, journal, "0.050000", "0.050000")
	envelope, ok = daily.invokeTool(rig, fmt.Sprintf("mjs-daily-%d", time.Now().UnixNano()))
	if ok {
		t.Fatalf("over-daily invoke succeeded: %v", envelope)
	}
	detail, _ = envelope["detail"].(map[string]any)
	if detail == nil || detail["reason"] != "over_daily_leash" {
		t.Fatalf("daily-leash surface missing reason: %v", envelope)
	}
	if bal, _ := rig.harness.BalanceMicro(ctx, rig.payeeDID); bal != 31_500 {
		t.Fatalf("payee balance moved past the daily leash: %d", bal)
	}
}
