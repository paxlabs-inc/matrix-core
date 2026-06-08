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
