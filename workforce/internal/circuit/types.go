// Package circuit owns durable provider, skill, operation, and effect breakers.
package circuit

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

var (
	// ErrOpen means an applicable breaker denies admission.
	ErrOpen = errors.New("circuit breaker is open")
	// ErrUncertain means breaker authority cannot be established and fails closed.
	ErrUncertain = errors.New("circuit breaker state is uncertain")
)

// Kind identifies one independently observable breaker dimension.
type Kind string

const (
	// KindProvider isolates failures of one external provider.
	KindProvider Kind = "provider"
	// KindSkill isolates failures of one versioned skill.
	KindSkill Kind = "skill"
	// KindEffectClass isolates failures of one consequence class.
	KindEffectClass Kind = "effect_class"
)

// Valid reports whether the breaker kind is recognized.
func (kind Kind) Valid() bool {
	return kind == KindProvider || kind == KindSkill || kind == KindEffectClass
}

// State is the closed durable circuit lifecycle.
type State string

const (
	// StateClosed admits normal execution.
	StateClosed State = "closed"
	// StateOpen denies execution until RetryAt.
	StateOpen State = "open"
	// StateHalfOpen admits only a bounded set of reversible/read-only probes.
	StateHalfOpen State = "half_open"
)

// Valid reports whether state is recognized.
func (state State) Valid() bool {
	return state == StateClosed || state == StateOpen || state == StateHalfOpen
}

// Config defines deterministic breaker thresholds and recovery bounds.
type Config struct {
	FailureThreshold uint32
	SuccessThreshold uint32
	Window           time.Duration
	OpenDuration     time.Duration
	HalfOpenLimit    uint32
	TrialDuration    time.Duration
}

// Validate rejects unbounded or nonsensical breaker policies.
func (config Config) Validate() error {
	if config.FailureThreshold == 0 || config.FailureThreshold > 1000 ||
		config.SuccessThreshold == 0 || config.SuccessThreshold > 1000 ||
		config.HalfOpenLimit == 0 || config.HalfOpenLimit > 100 {
		return fmt.Errorf("circuit: thresholds must be positive and bounded")
	}
	if config.Window <= 0 || config.Window > 24*time.Hour ||
		config.OpenDuration <= 0 || config.OpenDuration > 24*time.Hour ||
		config.TrialDuration <= 0 || config.TrialDuration > 30*time.Minute {
		return fmt.Errorf("circuit: durations must be positive and bounded")
	}
	return nil
}

// Key identifies one organization-scoped circuit.
type Key struct {
	OrganizationID contracts.OrganizationID
	Kind           Kind
	Name           string
}

// Validate rejects incomplete or unrecognized breaker identities.
func (key Key) Validate() error {
	if err := validateToken("organization_id", string(key.OrganizationID)); err != nil {
		return err
	}
	if !key.Kind.Valid() {
		return fmt.Errorf("circuit: invalid breaker kind %q", key.Kind)
	}
	return validateToken("breaker_name", key.Name)
}

// Snapshot is the durable observable circuit projection.
type Snapshot struct {
	Key
	State         State
	FailureCount  uint32
	SuccessCount  uint32
	HalfOpenInUse uint32
	Reason        string
	RetryAt       *time.Time
	Version       uint64
	UpdatedAt     time.Time
}

// Permit is kernel-minted admission authority and cannot be constructed by a seat.
type Permit struct {
	ID        string
	Keys      []Key
	ExpiresAt time.Time
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("circuit: %s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("circuit: %s contains an invalid character", name)
	}
	return nil
}
