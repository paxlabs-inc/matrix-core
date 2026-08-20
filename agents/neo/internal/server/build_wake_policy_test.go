package server

import (
	"strings"
	"testing"

	"centra/agents/neo/internal/codingruntime"
)

func TestBuildWakePreservesNeoOwnershipAndFrontendDelivery(t *testing.T) {
	prompt := buildWakePrompt(codingruntime.Job{}, codingruntime.WakeEnvelope{})
	for _, want := range []string{
		"Your durable Build run",
		"You are resuming your own coding work",
		"there is no separate builder",
		"frontend Paxeer Cloud post-build delivery rule",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("Build wake prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{"private coding sub-agent", "private worker"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("Build wake prompt exposes a separate builder: %q", forbidden)
		}
	}
}
