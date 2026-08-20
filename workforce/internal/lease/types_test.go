package lease

import (
	"errors"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
)

func TestRequest_RejectsInvalidAuthorityAndBounds(t *testing.T) {
	valid := testRequest()
	cases := []Request{
		func() Request { value := valid; value.ID = ""; return value }(),
		func() Request { value := valid; value.WakeID = ""; return value }(),
		func() Request { value := valid; value.OrganizationID = ""; return value }(),
		func() Request { value := valid; value.SeatID = ""; return value }(),
		func() Request { value := valid; value.NodeID = ""; return value }(),
		func() Request { value := valid; value.MandateID = ""; return value }(),
		func() Request { value := valid; value.MandateVersion = 0; return value }(),
		func() Request { value := valid; value.Policies = nil; return value }(),
		func() Request {
			value := valid
			value.Policies = append(value.Policies, value.Policies[0])
			return value
		}(),
		func() Request {
			value := valid
			value.Policies = append([]contracts.PolicyRef(nil), valid.Policies...)
			value.Policies[0].Hash.Algorithm = "bad"
			return value
		}(),
		func() Request { value := valid; value.IssuedAt = time.Time{}; return value }(),
		func() Request { value := valid; value.ExpiresAt = value.IssuedAt; return value }(),
		func() Request {
			value := valid
			value.ExpiresAt = value.IssuedAt.Add(2*time.Hour + time.Second)
			return value
		}(),
	}
	for index, candidate := range cases {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("case %d accepted: %+v", index, candidate)
		}
	}
	badToken := valid
	badToken.ID = "bad token"
	if err := badToken.Validate(); err == nil {
		t.Fatal("invalid token character accepted")
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	for _, state := range []State{StateActive, StateExpired, StateCancelled, StateLost} {
		if !state.Valid() {
			t.Fatalf("valid state rejected: %s", state)
		}
	}
	if State("unknown").Valid() {
		t.Fatal("unknown state accepted")
	}
}

func TestPolicyBindingHash_IsOrderIndependentAndContentSensitive(t *testing.T) {
	request := testRequest()
	left := policyBindingHash(request.Policies)
	request.Policies = append(request.Policies, contracts.PolicyRef{
		ID: "policy:second", Version: 1,
		Hash: contracts.ContentHash{Algorithm: "sha256", Digest: digest("b")},
	})
	right := policyBindingHash([]contracts.PolicyRef{request.Policies[1], request.Policies[0]})
	if policyBindingHash(request.Policies) != right {
		t.Fatal("policy binding hash depends on input order")
	}
	if left == right {
		t.Fatal("policy binding hash ignored added authority")
	}
}

func TestErrors_AreDistinctForCallerBranching(t *testing.T) {
	values := []error{ErrHeld, ErrStaleFence, ErrExpired, ErrCancelled, ErrPolicyMismatch, ErrUncertain}
	for left := range values {
		for right := range values {
			if left != right && errors.Is(values[left], values[right]) {
				t.Fatalf("%v aliases %v", values[left], values[right])
			}
		}
	}
}

func testRequest() Request {
	issued := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	return Request{
		ID: "lease:test", WakeID: "wake:test", OrganizationID: "organization:test",
		SeatID: "seat:test", NodeID: "node:test", MandateID: "mandate:test",
		MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID: "policy:test", Version: 1,
			Hash: contracts.ContentHash{Algorithm: "sha256", Digest: digest("a")},
		}},
		IssuedAt: issued, ExpiresAt: issued.Add(time.Hour),
	}
}

func digest(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
