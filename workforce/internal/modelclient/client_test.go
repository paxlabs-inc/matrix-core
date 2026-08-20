package modelclient

import (
	"bytes"
	"math"
	"testing"
	"time"

	neoprovider "centra/agents/neo/provider"
)

func TestNewBuildsCanonicalSamplingBinding(t *testing.T) {
	base := Config{
		Provider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
		ModelVersion: neoprovider.MiMoV25ProModel,
		Endpoint:     neoprovider.MiMoChatEndpoint, APIKey: "configured",
		ActorDID:    "did:matrix:workforce-test",
		Temperature: neoprovider.MiMoTemperature,
		MaxTokens:   1024, Timeout: time.Second,
	}
	first, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.Binding("model:test") != second.Binding("model:test") {
		t.Fatal("identical sampling configuration produced different bindings")
	}
	changed := base
	changed.MaxTokens = 2048
	third, err := New(changed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Binding("model:test").SamplingDigest ==
		third.Binding("model:test").SamplingDigest {
		t.Fatal("material sampling change retained the old binding")
	}
}

func TestCanonicalModelOutputNormalizesNestedProposalInput(t *testing.T) {
	output, err := canonicalModelOutput(`{
		"schema_version": "workforce.v1",
		"proposal": {"input": {"count": 1, "items": ["a", "b"]}}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"proposal":{"input":{"count":1,"items":["a","b"]}},"schema_version":"workforce.v1"}`)
	if !bytes.Equal(output, want) {
		t.Fatalf("canonical output = %s", output)
	}
	if _, err := canonicalModelOutput(`{} {}`); err == nil {
		t.Fatal("accepted trailing model decision")
	}
}

func TestNewRejectsNonFiniteTemperature(t *testing.T) {
	for _, temperature := range []float64{
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		_, err := New(Config{
			Provider: "mimo", ModelID: neoprovider.MiMoV25ProModel,
			ModelVersion: neoprovider.MiMoV25ProModel,
			Endpoint:     neoprovider.MiMoChatEndpoint, APIKey: "configured",
			ActorDID:    "did:matrix:workforce-test",
			Temperature: temperature, MaxTokens: 1024,
			Timeout: time.Second,
		})
		if err == nil {
			t.Fatalf("accepted non-finite temperature %v", temperature)
		}
	}
}
