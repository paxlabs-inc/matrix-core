package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"centra/agents/neo/internal/llm"
)

func TestObservableErrorClassificationAndFingerprint(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		class string
	}{
		{name: "cancelled", err: context.Canceled, class: "cancelled"},
		{name: "timeout", err: context.DeadlineExceeded, class: "timeout"},
		{name: "rate limited", err: fmt.Errorf("wrapped: %w", llm.ErrRateLimited), class: "rate_limited"},
		{name: "provider unavailable", err: fmt.Errorf("wrapped: %w", llm.ErrProviderUnavailable), class: "provider_unavailable"},
		{name: "internal", err: errors.New("deterministic internal sentinel"), class: "internal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := observableErrorClass(test.err); got != test.class {
				t.Fatalf("class = %q, want %q", got, test.class)
			}
			first := observableErrorFingerprint(test.err)
			second := observableErrorFingerprint(test.err)
			if first == "" || first != second || len(first) != 20 {
				t.Fatalf("fingerprint = %q then %q", first, second)
			}
			if first == test.err.Error() {
				t.Fatal("fingerprint exposed the error message")
			}
		})
	}
}
