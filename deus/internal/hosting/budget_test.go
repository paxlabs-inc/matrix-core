package hosting_test

import (
	"context"
	"testing"

	"github.com/paxlabs-inc/deus/internal/hosting"
)

func TestBudgetKillSwitchBlocksDeploy(t *testing.T) {
	limits := hosting.Limits{KillSwitch: true, MaxAlwaysWarm: 2}
	b := hosting.NewBudget(nil, limits)
	if err := b.AllowNewDeployment(context.Background(), false); err == nil {
		t.Fatal("expected kill-switch error for scale-to-zero deploy")
	}
}

func TestBudgetKillSwitchBlocksAlwaysWarm(t *testing.T) {
	// AllowAlwaysWarm short-circuits on the kill switch before touching the
	// store, so a nil store is sufficient for this unit test.
	b := hosting.NewBudget(nil, hosting.Limits{KillSwitch: true, MaxAlwaysWarm: 5})
	if err := b.AllowNewDeployment(context.Background(), true); err == nil {
		t.Fatal("expected kill-switch error for always-warm deploy")
	}
	if err := b.AllowAlwaysWarm(context.Background()); err == nil {
		t.Fatal("expected kill-switch error from AllowAlwaysWarm")
	}
}

func TestBudgetScaleToZeroAllowedWithoutKillSwitch(t *testing.T) {
	// Scale-to-zero deploys do not consult the store when the kill switch is
	// off, so this stays Postgres-free.
	b := hosting.NewBudget(nil, hosting.Limits{KillSwitch: false, MaxAlwaysWarm: 1})
	if err := b.AllowNewDeployment(context.Background(), false); err != nil {
		t.Fatalf("expected scale-to-zero deploy allowed, got %v", err)
	}
}
