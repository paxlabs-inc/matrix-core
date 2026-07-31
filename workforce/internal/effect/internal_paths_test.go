package effect

import (
	"context"
	"errors"
	"testing"

	"matrix/workforce/internal/skills"
)

func TestGateway_SetStateRejectsUnknownStateBeforePersistence(t *testing.T) {
	gateway := &Gateway{}
	if err := gateway.setState(context.Background(), Proposal{}, "unknown", "", ""); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func TestGateway_ProbeSettersRejectUnknownOutcomeBeforePersistence(t *testing.T) {
	gateway := &Gateway{}
	if err := gateway.setProbe(context.Background(), Proposal{}, "other", "", ""); err == nil {
		t.Fatal("unknown probe outcome accepted")
	}
	if err := gateway.setProbeOutcome(context.Background(), Proposal{}, "other"); err == nil {
		t.Fatal("unknown terminal probe outcome accepted")
	}
	if (ProbeResult{Outcome: skills.ProbeUnknown}).Outcome != skills.ProbeUnknown {
		t.Fatal("probe result outcome changed")
	}
}

func TestErrorTaxonomy_RemainsDistinct(t *testing.T) {
	values := []error{ErrConflict, ErrAmbiguous, ErrRejected, ErrUncertain}
	for left := range values {
		for right := range values {
			if left != right && errors.Is(values[left], values[right]) {
				t.Fatalf("%v aliases %v", values[left], values[right])
			}
		}
	}
}
