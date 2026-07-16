// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package episodicsweep

import (
	"fmt"
	"time"

	"github.com/paxlabs-inc/chronos/internal/schedule"
	"github.com/paxlabs-inc/chronos/pkg/types"
)

const WakeMessage = "DEJA_VU_SWEEP: idle backfill trigger. Build bounded transcript lexical postings and provenance links; produce no chat reply."
const DefaultCronExpr = "@every 1h"

type Spec struct {
	ConversationID string
	CronExpr       string
	Timezone       string
	Label          string
	IdempotencyKey string
	MaxFailures    int
}

func BuildAlarm(spec Spec, now time.Time) (types.Alarm, error) {
	if spec.ConversationID == "" {
		return types.Alarm{}, fmt.Errorf("episodicsweep: conversation_id is required")
	}
	cronExpr := spec.CronExpr
	if cronExpr == "" {
		cronExpr = DefaultCronExpr
	}
	tz := spec.Timezone
	if tz == "" {
		tz = "UTC"
	}
	next, err := schedule.NextCron(cronExpr, tz, now)
	if err != nil {
		return types.Alarm{}, fmt.Errorf("episodicsweep: resolve next fire: %w", err)
	}
	return types.Alarm{Kind: types.KindCron, Label: spec.Label, CronExpr: cronExpr, Timezone: tz, NextFireAt: next, ConversationID: spec.ConversationID, WakeMessage: WakeMessage, Status: types.StatusActive, IdempotencyKey: spec.IdempotencyKey, MaxFailures: spec.MaxFailures}, nil
}
