package lxp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nodeMWServer = `
import http from 'node:http'
import { createLXP } from process.env.LXP_JS

const lxp = createLXP({ layerxUrl: process.env.LXP_URL, keyHex: process.env.LXP_KEY, didLabel: 'node-mw' })
let count = 0
const price = (req) => {
  if (req.url === '/exact') return { amount_usdx: '0.031500', pay_to: process.env.PAYEE, mode: 'exact' }
  if (req.url === '/hold') return { amount_usdx: '0.031500', pay_to: process.env.PAYEE, mode: 'hold', ttl_s: 60 }
  if (req.url === '/holdfail') return { amount_usdx: '0.031500', pay_to: process.env.PAYEE, mode: 'hold', ttl_s: 60 }
  return null
}
const handler = async (req, res) => {
  if (req.url === '/count') {
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ count }))
    return
  }
  count++
  if (req.url === '/holdfail') {
    res.writeHead(500)
    res.end('boom')
    return
  }
  res.writeHead(200, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify({ served: true, path: req.url }))
}
const guarded = lxp.guard(price, handler)
const server = http.createServer((req, res) => {
  guarded(req, res).catch((e) => {
    res.writeHead(500)
    res.end(String(e))
  })
})
server.listen(0, '127.0.0.1', () => console.log('PORT=' + server.address().port))
`

// startNodeMW boots the REAL runner-harness Node middleware (runner/src/lxp.js)
// wrapping a live handler, wired at the given layerxd. Returns its base URL.
func startNodeMW(t *testing.T, layerxURL, keyHex, payeeDID string) string {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping node middleware test")
	}
	lxpJS, err := filepath.Abs(filepath.Join("..", "..", "runner", "src", "lxp.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(nodeMWServer, "process.env.LXP_JS", fmt.Sprintf("%q", lxpJS), 1)
	dir := t.TempDir()
	path := filepath.Join(dir, "mw-server.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nodeBin, path)
	cmd.Env = append(os.Environ(),
		"LXP_URL="+layerxURL,
		"LXP_KEY="+keyHex,
		"PAYEE="+payeeDID,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node middleware: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	sc := bufio.NewScanner(stdout)
	portCh := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if p, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "PORT="); ok {
				portCh <- p
				return
			}
		}
	}()
	select {
	case p := <-portCh:
		return "http://127.0.0.1:" + p
	case <-time.After(15 * time.Second):
		t.Fatal("node middleware never reported its port")
		return ""
	}
}

func mwGet(t *testing.T, base, path, callerDID, paymentHeader string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	if callerDID != "" {
		req.Header.Set(HeaderCallerDID, callerDID)
	}
	if paymentHeader != "" {
		req.Header.Set(HeaderPayment, paymentHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func mwCount(t *testing.T, base string) int {
	t.Helper()
	resp := mwGet(t, base, "/count", "", "")
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("count: %v", err)
	}
	return out.Count
}

// TestNodeMiddlewareAgainstRealLayerxd proves req.10.2: the runner-harness
// Node middleware speaks the same lxp/1 protocol as pkg/lxp — Go-signed
// payments (the shared vectors' semantics) settle through it against REAL
// layerxd in both modes, invalid payments re-challenge, failed executions
// release their holds, and layerxd-down is 503, never a free call.
func TestNodeMiddlewareAgainstRealLayerxd(t *testing.T) {
	harness, lxd, ctx := newLayerxd(t)
	payer := newTestPayer(t)
	if err := harness.CreditDeposit(ctx, payer.did, "0xabc", "0xdep-"+payer.did, 1_000_000); err != nil {
		t.Fatalf("fund payer: %v", err)
	}
	seed := "8080808080808080808080808080808080808080808080808080808080808080"
	payee := newTestPayer(t)
	base := startNodeMW(t, lxd.URL, seed, payee.did)

	// 1. Exact: 402 terms -> Go-signed retry -> 200 + receipt, payee credited.
	resp := mwGet(t, base, "/exact", payer.did, "")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("unpaid exact = %d, want 402", resp.StatusCode)
	}
	_, terms := decodeChallenge(t, resp)
	if terms.Protocol != Protocol || terms.AmountUSDX != "0.031500" || terms.Mode != ModeExact ||
		terms.Nonce == "" || terms.LayerX != lxd.URL {
		t.Fatalf("bad exact terms: %+v", terms)
	}
	resp = mwGet(t, base, "/exact", payer.did, payer.signPayment(t, terms))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid exact = %d", resp.StatusCode)
	}
	rcpt, err := DecodeReceipt(resp.Header.Get(HeaderReceipt))
	resp.Body.Close()
	if err != nil || rcpt.Seq <= 0 || rcpt.AmountUSDX != "0.031500" {
		t.Fatalf("bad exact receipt: %+v (%v)", rcpt, err)
	}
	if bal, _ := harness.BalanceMicro(ctx, payee.did); bal != 31_500 {
		t.Fatalf("payee after exact = %d, want 31500", bal)
	}
	if n := mwCount(t, base); n != 1 {
		t.Fatalf("executions after exact = %d, want 1", n)
	}

	// 2. Hold: terms carry the node service's captor DID + ttl; capture on 2xx.
	resp = mwGet(t, base, "/hold", payer.did, "")
	_, holdTerms := decodeChallenge(t, resp)
	if holdTerms.Mode != ModeHold || holdTerms.CaptorDID == "" || holdTerms.TTLSeconds != 60 {
		t.Fatalf("bad hold terms: %+v", holdTerms)
	}
	resp = mwGet(t, base, "/hold", payer.did, payer.signPayment(t, holdTerms))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("paid hold = %d", resp.StatusCode)
	}
	rcpt, err = DecodeReceipt(resp.Header.Get(HeaderReceipt))
	resp.Body.Close()
	if err != nil || rcpt.Seq <= 0 {
		t.Fatalf("bad hold receipt: %+v (%v)", rcpt, err)
	}
	if bal, _ := harness.BalanceMicro(ctx, payee.did); bal != 63_000 {
		t.Fatalf("payee after hold = %d, want 63000", bal)
	}

	// 3. Failed execution behind a hold: 500 flushes, hold releases in full.
	before, _ := harness.BalanceMicro(ctx, payer.did)
	resp = mwGet(t, base, "/holdfail", payer.did, "")
	_, failTerms := decodeChallenge(t, resp)
	resp = mwGet(t, base, "/holdfail", payer.did, payer.signPayment(t, failTerms))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed execution = %d, want 500 flushed", resp.StatusCode)
	}
	if resp.Header.Get(HeaderReceipt) != "" {
		t.Fatal("failed execution carried a receipt")
	}
	resp.Body.Close()
	if after, _ := harness.BalanceMicro(ctx, payer.did); after != before {
		t.Fatalf("payer balance moved on failed execution: %d -> %d", before, after)
	}
	if bal, _ := harness.BalanceMicro(ctx, payee.did); bal != 63_000 {
		t.Fatalf("payee credited for failed execution: %d", bal)
	}

	// 4. Tampered payment -> fresh 402 terms_mismatch, no execution.
	execs := mwCount(t, base)
	resp = mwGet(t, base, "/exact", payer.did, "")
	_, tamperTerms := decodeChallenge(t, resp)
	pay, _ := ParsePayment(payer.signPayment(t, tamperTerms))
	pay.AmountUSDX = "0.000001"
	resp = mwGet(t, base, "/exact", payer.did, EncodePayment(pay))
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("tampered = %d, want 402", resp.StatusCode)
	}
	reason, _ := decodeChallenge(t, resp)
	if reason != ReasonTermsMismatch {
		t.Fatalf("tampered reason = %q", reason)
	}
	if n := mwCount(t, base); n != execs {
		t.Fatalf("tampered payment executed (%d -> %d)", execs, n)
	}

	// 5. layerxd down: signed or not, 503 payment_unavailable — never free.
	resp = mwGet(t, base, "/exact", payer.did, "")
	_, downTerms := decodeChallenge(t, resp)
	lxd.Close()
	resp = mwGet(t, base, "/exact", payer.did, payer.signPayment(t, downTerms))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rail-down paid = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	resp = mwGet(t, base, "/exact", payer.did, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("rail-down unpaid = %d, want 503", resp.StatusCode)
	}
	resp.Body.Close()
	if n := mwCount(t, base); n != execs {
		t.Fatalf("rail-down served a free call (%d -> %d)", execs, n)
	}
}
