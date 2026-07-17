package chain

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPaxeerSettlerLiveRecoversHistoricalIdentityFromIndexedHistory(t *testing.T) {
	rpcURL := os.Getenv("LAYERX_CHAIN_RPC")
	historyURL := os.Getenv("LAYERX_ANCHOR_HISTORY_URL")
	if rpcURL == "" || historyURL == "" {
		t.Skip("LAYERX_CHAIN_RPC and LAYERX_ANCHOR_HISTORY_URL are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client, err := NewClient(ctx, rpcURL, DefaultChainID)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	history, err := NewBlockscoutAnchorHistory(historyURL, nil)
	if err != nil {
		t.Fatalf("NewBlockscoutAnchorHistory: %v", err)
	}
	settler, err := NewPaxeerSettler(
		client,
		testOperator(t),
		"0xf756895fD414f7D20413B61c9291ABe98fcED1CE",
		"0x63F317750ff18272565249345c63E5688501EA1D",
		history,
	)
	if err != nil {
		t.Fatalf("NewPaxeerSettler: %v", err)
	}

	fixtures := []struct {
		root string
		tx   string
	}{
		{
			root: "013b4af850ef8c64673fbbdecdb3878ebb4a31c5f77f0f054222dbf56e5a6311",
			tx:   "0x11bcdf51eb0d72324dbe2520169ca68cfeaa06976c71c5782105ebf56e4f5250",
		},
		{
			root: "b08943587a61503e853a71ce9863dab9dbbcb4b92b124a4dc84391242d832159",
			tx:   "0xff6c394f5297a1af7776b4a8715bbf95d033d6468ae167be0b583579d76c7a66",
		},
		{
			root: "65cfcabd62d309d03846c199aab335eeda80bda877ccafba497cde7cf2f79301",
			tx:   "0x8a1a341673baffcf66795c1cc45589b3e2cab921093e41fb320eb3c416367259",
		},
		{
			root: "ec57f4c3841b72a9f3abf6b0b54df6da4a506c178fb68064c27b459c9f1ddaf6",
			tx:   "0xa7d95dd030d956379f2f893dc836f14d5b0bb9836974f751af85b77e15257c45",
		},
	}
	firstBatch, err := settler.buildBatch(fixtures[0].root, time.Time{}, nil)
	if err != nil {
		t.Fatalf("buildBatch: %v", err)
	}
	anchorEvent := settler.bind.anchor.Events["SettlementAnchored"]
	if _, err := settler.findAnchorTxRPC(ctx, anchorEvent.ID, firstBatch.BatchId); err == nil {
		t.Fatal("production primary RPC unexpectedly retained the historical anchor log; pruned-RPC regression was not exercised")
	}
	for _, fixture := range fixtures {
		got, err := settler.AnchorBatch(ctx, fixture.root, 0, time.Time{}, nil)
		if err != nil {
			t.Fatalf("AnchorBatch(%s): %v", fixture.root, err)
		}
		if got != fixture.tx {
			t.Fatalf("AnchorBatch(%s) tx = %s, want original %s", fixture.root, got, fixture.tx)
		}
	}
}
