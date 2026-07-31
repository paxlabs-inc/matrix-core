// Package lease owns linearizable wake authority and monotonically increasing
// fencing tokens.
package lease

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
)

var (
	// ErrHeld means the seat or graph node already has an active lease.
	ErrHeld = errors.New("lease scope is already held")
	// ErrStaleFence means the caller no longer owns the current generation.
	ErrStaleFence = errors.New("lease fence is stale")
	// ErrExpired means lease authority has expired.
	ErrExpired = errors.New("lease has expired")
	// ErrCancelled means lease authority was explicitly cancelled.
	ErrCancelled = errors.New("lease was cancelled")
	// ErrPolicyMismatch means bound authority is no longer current.
	ErrPolicyMismatch = errors.New("lease policy binding is invalid")
	// ErrUncertain means the lease service cannot prove authority and fails closed.
	ErrUncertain = errors.New("lease authority is uncertain")
)

// State is the closed durable lease lifecycle.
type State string

const (
	// StateActive authorizes bounded work until expiry.
	StateActive State = "active"
	// StateExpired is terminal time-based loss of authority.
	StateExpired State = "expired"
	// StateCancelled is terminal explicit revocation.
	StateCancelled State = "cancelled"
	// StateLost records fail-closed authority uncertainty.
	StateLost State = "lost"
)

// Valid reports whether state is executable by this release.
func (state State) Valid() bool {
	return state == StateActive || state == StateExpired ||
		state == StateCancelled || state == StateLost
}

// Request is a complete acquisition request for one seat and graph node.
type Request struct {
	ID             contracts.LeaseID
	WakeID         contracts.WakeID
	OrganizationID contracts.OrganizationID
	SeatID         contracts.SeatID
	NodeID         dependency.NodeID
	MandateID      contracts.MandateID
	MandateVersion uint64
	Policies       []contracts.PolicyRef
	IssuedAt       time.Time
	ExpiresAt      time.Time
}

// Validate rejects incomplete, expired, unbounded, or duplicate policy bindings.
func (request Request) Validate() error {
	for name, value := range map[string]string{
		"lease_id": string(request.ID), "wake_id": string(request.WakeID),
		"organization_id": string(request.OrganizationID), "seat_id": string(request.SeatID),
		"node_id": string(request.NodeID), "mandate_id": string(request.MandateID),
	} {
		if err := validateToken(name, value); err != nil {
			return err
		}
	}
	if request.MandateVersion == 0 {
		return fmt.Errorf("mandate version must be positive")
	}
	if len(request.Policies) == 0 || len(request.Policies) > 64 {
		return fmt.Errorf("lease must bind 1 to 64 policies")
	}
	seen := make(map[contracts.PolicyID]bool, len(request.Policies))
	for _, policy := range request.Policies {
		if err := policy.Validate(); err != nil {
			return err
		}
		if seen[policy.ID] {
			return fmt.Errorf("policy %q is duplicated", policy.ID)
		}
		seen[policy.ID] = true
	}
	if !validUTC(request.IssuedAt) || !validUTC(request.ExpiresAt) ||
		!request.ExpiresAt.After(request.IssuedAt) ||
		request.ExpiresAt.Sub(request.IssuedAt) > 2*time.Hour {
		return fmt.Errorf("lease times must be UTC, ordered, and at most two hours")
	}
	return nil
}

// Grant is the durable lease projection returned by the authority service.
type Grant struct {
	Request
	Fence     contracts.FenceToken
	State     State
	RenewedAt *time.Time
}

// Incident is a safe typed explanation of failed authorization.
type Incident struct {
	ID             string
	OrganizationID contracts.OrganizationID
	LeaseID        contracts.LeaseID
	Kind           string
	Reason         string
	CreatedAt      time.Time
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
