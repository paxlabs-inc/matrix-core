package projectbrain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIntegration_CodeGraphUltraCurrentIndex(t *testing.T) {
	executable := os.Getenv("WORKFORCE_CODEGRAPH_ULTRA_LIVE_EXECUTABLE")
	if executable == "" {
		t.Skip("set WORKFORCE_CODEGRAPH_ULTRA_LIVE_EXECUTABLE to test CodeGraph Ultra")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := NewCodeGraph(executable, func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	snapshot, err := graph.CaptureFiles(
		ctx, root, []string{"workforce/internal/wakeruntime/recovery.go"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Fresh || len(snapshot.Files) != 1 || len(snapshot.PendingFiles) != 0 {
		t.Fatalf("CodeGraph Ultra snapshot is not fresh: %#v", snapshot)
	}
	impact, err := graph.Impact(ctx, root, "RunClaim", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.Affected) == 0 {
		t.Fatal("CodeGraph Ultra impact projection is empty")
	}
	if _, err := graph.TestsAffected(
		ctx, root, []string{"workforce/internal/wakeruntime/recovery.go"}, 8,
	); err != nil {
		t.Fatal(err)
	}
}
