package receipts_test

import (
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/receipts"
)

func TestSignAndVerifyQuote(t *testing.T) {
	key := "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	signer, err := receipts.NewSignerFromHex(31337, "0x5FbDB2315678afecb367f032d93F642f64180aa3", key)
	if err != nil {
		t.Fatal(err)
	}
	fields := receipts.QuoteFields{
		ServiceID:      "svc-1",
		EndpointID:     "ep-1",
		PricingVersion: 1,
		UnitPriceWei:   "200000000000000",
		MaxUnits:       "1",
		Caller:         "did:matrix:test",
		ExpiresAt:      time.Unix(1700000000, 0).UTC(),
	}
	digest, sig, err := signer.SignQuote(fields)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || sig == "" {
		t.Fatal("empty digest or sig")
	}
	if err := signer.VerifyQuote(digest, sig); err != nil {
		t.Fatal(err)
	}
}
