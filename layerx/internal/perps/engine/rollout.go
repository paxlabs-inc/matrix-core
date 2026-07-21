package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/paxlabs-inc/layerx/internal/store"
)

// StoreRolloutAdmission reads the singleton rollout state for every ACTIVE
// risk increase. The state is deliberately not cached: a rollback takes effect
// on the next intent without a daemon restart.
type StoreRolloutAdmission struct {
	Store     *store.Store
	StaffDIDs map[string]bool
}

func (a StoreRolloutAdmission) Admit(ctx context.Context, ownerDID, actingDID string) error {
	if a.Store == nil {
		return ErrRolloutDenied
	}
	state, err := a.Store.GetPerpRolloutState(ctx)
	if err != nil {
		return ErrRolloutDenied
	}
	delegated := actingDID != "" && actingDID != ownerDID
	switch state.Stage {
	case "STAFF":
		if !a.StaffDIDs[ownerDID] || delegated {
			return ErrRolloutDenied
		}
	case "PERCENT":
		if state.TrafficPercent <= 0 || RolloutCohort(ownerDID) >= state.TrafficPercent {
			return ErrRolloutDenied
		}
		if delegated && !state.AgentsEnabled {
			return ErrRolloutDenied
		}
	case "FULL", "RETIRE_READY", "RETIRED":
		if delegated && !state.AgentsEnabled {
			return ErrRolloutDenied
		}
	default:
		return ErrRolloutDenied
	}
	return nil
}

// RolloutCohort returns the stable 0-99 bucket used throughout a percentage
// ramp. The domain separator prevents accidental coupling to other hashes.
func RolloutCohort(ownerDID string) int {
	sum := sha256.Sum256([]byte("layerx.perps.rollout.v1\x00" + ownerDID))
	return int(binary.BigEndian.Uint64(sum[:8]) % 100)
}
