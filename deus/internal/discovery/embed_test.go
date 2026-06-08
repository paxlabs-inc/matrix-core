package discovery_test

import (
	"context"
	"testing"

	"github.com/paxlabs-inc/deus/internal/discovery"
)

func TestHashEmbedderDeterministic(t *testing.T) {
	e := discovery.NewHashEmbedder()
	a, err := e.Embed(context.Background(), "weather forecast")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(context.Background(), "weather forecast")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != e.Dim() || len(a) != len(b) {
		t.Fatalf("dim mismatch %d %d %d", len(a), len(b), e.Dim())
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("non-deterministic embed")
		}
	}
}

func TestHashEmbedderNotSemantic(t *testing.T) {
	if discovery.NewHashEmbedder().Semantic() {
		t.Fatal("hash stub should not enable vector search path")
	}
}
