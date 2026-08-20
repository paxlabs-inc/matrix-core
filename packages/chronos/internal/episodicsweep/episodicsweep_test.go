// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package episodicsweep

import (
	"strings"
	"testing"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/chronos/pkg/types"
)

func TestAlarmIsRecurringBoundedSweep(t *testing.T) {
	a, err := BuildAlarm(Spec{ConversationID: "conv-sweep", IdempotencyKey: "deja-vu"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != types.KindCron || a.CronExpr != DefaultCronExpr || !strings.Contains(a.WakeMessage, "DEJA_VU_SWEEP") {
		t.Fatalf("alarm=%+v", a)
	}
	if _, err := BuildAlarm(Spec{}, time.Now()); err == nil {
		t.Fatal("empty conversation accepted")
	}
}
