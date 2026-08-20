package memory

import "testing"

func TestFinancialAutonomyBoundary(t *testing.T) {
	for _, text := range []string{"transfer funds", "buy ETH", "send payment"} {
		if !ClassifyFinancial(text) {
			t.Fatalf("expected financial classification for %q", text)
		}
	}
	for _, text := range []string{"research token pricing", "write a market analysis", "summarize the API thread"} {
		if ClassifyFinancial(text) {
			t.Fatalf("analysis must remain non-actuating: %q", text)
		}
	}
}
