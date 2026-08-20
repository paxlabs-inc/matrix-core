// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type SharedAlarm struct {
	ID             string          `json:"id"`
	OwnerDID       string          `json:"owner_did"`
	Label          string          `json:"label"`
	Kind           string          `json:"kind"`
	FireAt         time.Time       `json:"fire_at"`
	CronExpr       string          `json:"cron_expr"`
	Timezone       string          `json:"timezone"`
	NextFireAt     time.Time       `json:"next_fire_at"`
	ConversationID string          `json:"conversation_id"`
	WakeMessage    string          `json:"wake_message"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	MaxFailures    int             `json:"max_failures"`
	Target         string          `json:"target"`
	Status         string          `json:"status"`
}

type ImportOptions struct {
	OwnerDID string
	DryRun   bool
}

type ImportResult struct {
	SourceID string `json:"source_id"`
	LocalID  string `json:"local_id,omitempty"`
	Action   string `json:"action"`
	Error    string `json:"error,omitempty"`
}

func (store *Store) ImportShared(ctx context.Context, alarms []SharedAlarm, options ImportOptions) []ImportResult {
	owner := strings.TrimSpace(options.OwnerDID)
	results := make([]ImportResult, 0, len(alarms))
	for _, shared := range alarms {
		result := ImportResult{SourceID: strings.TrimSpace(shared.ID)}
		if result.SourceID == "" || owner == "" || strings.TrimSpace(shared.OwnerDID) != owner {
			result.Action, result.Error = "rejected", "shared alarm ownership does not match the local owner"
			results = append(results, result)
			continue
		}
		if localID, ok, err := store.importMapping(ctx, "shared-chronos", result.SourceID); err != nil {
			result.Action, result.Error = "error", err.Error()
			results = append(results, result)
			continue
		} else if ok {
			result.Action, result.LocalID = "already_mapped", localID
			results = append(results, result)
			continue
		}
		if shared.IdempotencyKey != "" {
			if existing, err := store.FindByIdempotency(ctx, shared.IdempotencyKey); err == nil {
				result.Action, result.LocalID = "map_existing", existing.ID
				if !options.DryRun {
					if err := store.recordImportMapping(ctx, "shared-chronos", result.SourceID, existing.ID); err != nil {
						result.Action, result.Error = "error", err.Error()
					}
				}
				results = append(results, result)
				continue
			}
		}
		request, err := shared.createRequest()
		if err != nil {
			result.Action, result.Error = "rejected", err.Error()
			results = append(results, result)
			continue
		}
		result.Action, result.LocalID = "create", request.ID
		if options.DryRun {
			results = append(results, result)
			continue
		}
		created, _, err := store.Create(ctx, request)
		if err == nil {
			err = store.recordImportMapping(ctx, "shared-chronos", result.SourceID, created.ID)
		}
		if err != nil {
			result.Action, result.Error = "error", err.Error()
		} else {
			result.LocalID = created.ID
		}
		results = append(results, result)
	}
	return results
}

func (shared SharedAlarm) createRequest() (CreateRequest, error) {
	if shared.Status != "" && shared.Status != "active" && shared.Status != "scheduled" {
		return CreateRequest{}, fmt.Errorf("shared alarm status %q is not importable", shared.Status)
	}
	next := shared.NextFireAt.UTC()
	if next.IsZero() {
		next = shared.FireAt.UTC()
	}
	key := strings.TrimSpace(shared.IdempotencyKey)
	if key == "" {
		key = "shared-chronos:" + strings.TrimSpace(shared.ID)
	}
	timezone := strings.TrimSpace(shared.Timezone)
	if timezone == "" {
		timezone = "UTC"
	}
	request := CreateRequest{ID: strings.TrimSpace(shared.ID), IdempotencyKey: key, NextFire: next,
		CronExpr: strings.TrimSpace(shared.CronExpr), Timezone: timezone,
		MisfirePolicy: MisfireFireOnce,
		Body: Body{Payload: shared.Payload, WakeMessage: shared.WakeMessage, ConversationID: shared.ConversationID,
			Label: shared.Label, Target: shared.Target, MaxFailures: shared.MaxFailures}}
	if shared.Kind == "cron" || request.CronExpr != "" {
		request.MisfirePolicy = MisfireCoalesce
		if request.NextFire.IsZero() {
			now := time.Now().UTC()
			computed, err := nextScheduled(0, request.CronExpr, request.Timezone, now, now)
			if err != nil {
				return CreateRequest{}, err
			}
			request.NextFire = computed
		}
	}
	if err := validateCreate(request); err != nil {
		return CreateRequest{}, err
	}
	return request, nil
}
