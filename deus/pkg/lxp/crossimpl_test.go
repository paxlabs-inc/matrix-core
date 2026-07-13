package lxp

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCrossImplementationVectors proves DEUS-LAYERX req.9.3: the deus.mjs
// client half signs byte-identical canonical intent preimages to this
// package (and therefore to layerxd's auth.IntentMessage). The REAL bridge
// script computes the preimages and signatures; Go recomputes and verifies.
func TestCrossImplementationVectors(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping cross-implementation vector test")
	}
	script := filepath.Join("..", "..", "..", "tools", "deus", "deus.mjs")

	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pubHex := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	wantDID := "did:matrix:vector:" + pubHex[:16]

	cases := []map[string]any{
		{"mode": "exact", "pay_to": "did:matrix:payee:8899aabbccddeeff", "amount_usdx": "0.031500", "nonce": "nonce-exact-noref"},
		{"mode": "exact", "pay_to": "did:matrix:payee:8899aabbccddeeff", "amount_usdx": "12.500000", "nonce": "nonce-exact-ref",
			"ref": "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"},
		{"mode": "hold", "pay_to": "did:matrix:payee:8899aabbccddeeff", "amount_usdx": "0.031500", "nonce": "nonce-hold-ref",
			"ttl_s": int64(60), "captor_did": "did:matrix:deus-gateway:ffeeddccbbaa9988",
			"ref": "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"mode": "hold", "pay_to": "did:matrix:payee:8899aabbccddeeff", "amount_usdx": "1.000000", "nonce": "nonce-hold-noref",
			"ttl_s": int64(120), "captor_did": "did:matrix:deus-gateway:ffeeddccbbaa9988"},
	}
	input, err := json.Marshal(map[string]any{
		"seed":  hex.EncodeToString(seed),
		"label": "vector",
		"cases": cases,
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(nodeBin, script, "--vectors")
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("deus.mjs --vectors: %v (%s)", err, stderr.String())
	}
	var out struct {
		DID       string `json:"did"`
		PublicKey string `json:"public_key"`
		Results   []struct {
			Preimage string  `json:"preimage"`
			Payment  Payment `json:"payment"`
			Header   string  `json:"header"`
		} `json:"results"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode vectors: %v (%s)", err, stdout.String())
	}
	if out.DID != wantDID || out.PublicKey != pubHex {
		t.Fatalf("js identity = %s/%s, want %s/%s", out.DID, out.PublicKey, wantDID, pubHex)
	}
	if len(out.Results) != len(cases) {
		t.Fatalf("results = %d, want %d", len(out.Results), len(cases))
	}

	for i, res := range out.Results {
		c := cases[i]
		p := res.Payment
		var want string
		if c["mode"] == "hold" {
			want = HoldPreimage(p, c["ttl_s"].(int64), c["captor_did"].(string))
		} else {
			want = PayPreimage(p)
		}
		if res.Preimage != want {
			t.Fatalf("case %d preimage mismatch:\n  js: %s\n  go: %s", i, res.Preimage, want)
		}
		if err := verifyDIDSig(p.FromDID, p.PublicKey, p.Signature, want); err != nil {
			t.Fatalf("case %d signature does not verify in go: %v", i, err)
		}
		// The header the bridge sends parses back into the same payment.
		parsed, err := ParsePayment(res.Header)
		if err != nil || parsed != p {
			t.Fatalf("case %d header round trip: %+v (%v)", i, parsed, err)
		}
	}
}
