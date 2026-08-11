// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortexclient

import (
	"context"

	flatbuffers "matrix/cortexclient/internal/flatbuffers"
	"matrix/cortexclient/wire/neocortex/protocol"
)

// Health is the admin health summary.
type Health struct {
	Ready             bool
	ActorCount        uint32
	ActiveConnections uint32
}

// ProjectionStat is one projection's applied-LSN checkpoint.
type ProjectionStat struct {
	Name       string
	AppliedLsn uint64
}

// ActorStats summarizes one actor store.
type ActorStats struct {
	Actor       uint16
	LogEvents   uint64
	LogBytes    uint64
	Projections []ProjectionStat
}

// VerifyStatus is the engine's tamper-evidence status for one actor.
type VerifyStatus struct {
	Actor             uint16
	Root              [32]byte
	LeafCount         uint64
	LastCheckpointLsn uint64
	Verified          bool
}

// AdminHealth reads engine health (admin capability).
func (c *Client) AdminHealth(ctx context.Context) (Health, error) {
	table, _, err := c.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			protocol.HealthStart(builder)
			health := protocol.HealthEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadHealth, health)
		}, protocol.ResponsePayloadHealthResult)
	if err != nil {
		return Health{}, err
	}
	var result protocol.HealthResult
	result.Init(table.Bytes, table.Pos)
	return Health{
		Ready:             result.Ready(),
		ActorCount:        result.ActorCount(),
		ActiveConnections: result.ActiveConnections(),
	}, nil
}

// AdminStats reads one actor's log and projection statistics.
func (c *Client) AdminStats(ctx context.Context, actor uint16) (ActorStats, error) {
	table, _, err := c.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			protocol.StatsStart(builder)
			protocol.StatsAddActor(builder, actor)
			stats := protocol.StatsEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadStats, stats)
		}, protocol.ResponsePayloadStatsResult)
	if err != nil {
		return ActorStats{}, err
	}
	var result protocol.StatsResult
	result.Init(table.Bytes, table.Pos)
	stats := ActorStats{
		Actor:     result.Actor(),
		LogEvents: result.LogEvents(),
		LogBytes:  result.LogBytes(),
	}
	for index := 0; index < result.ProjectionStatsLength(); index++ {
		var stat protocol.ProjectionStat
		if !result.ProjectionStats(&stat, index) {
			return ActorStats{}, ErrProtocol
		}
		stats.Projections = append(stats.Projections, ProjectionStat{
			Name:       string(stat.Name()),
			AppliedLsn: stat.AppliedLsn(),
		})
	}
	return stats, nil
}

// AdminVerifyStatus reads the MMR verification status for one actor.
func (c *Client) AdminVerifyStatus(ctx context.Context, actor uint16) (VerifyStatus, error) {
	table, _, err := c.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			protocol.VerifyStatusStart(builder)
			protocol.VerifyStatusAddActor(builder, actor)
			verify := protocol.VerifyStatusEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadVerifyStatus, verify)
		}, protocol.ResponsePayloadVerifyResult)
	if err != nil {
		return VerifyStatus{}, err
	}
	var result protocol.VerifyResult
	result.Init(table.Bytes, table.Pos)
	status := VerifyStatus{
		Actor:             result.Actor(),
		LeafCount:         result.LeafCount(),
		LastCheckpointLsn: result.LastCheckpointLsn(),
		Verified:          result.Verified(),
	}
	copy(status.Root[:], result.RootBytes())
	return status, nil
}

// AdminRebuildProjection drops and replays a projection ("all" rebuilds every
// projection) and returns the applied LSN.
func (c *Client) AdminRebuildProjection(ctx context.Context, actor uint16, name string) (uint64, error) {
	table, _, err := c.request(ctx,
		func(builder *flatbuffers.Builder, requestID uint64) []byte {
			nameOffset := builder.CreateString(name)
			protocol.RebuildProjectionStart(builder)
			protocol.RebuildProjectionAddActor(builder, actor)
			protocol.RebuildProjectionAddName(builder, nameOffset)
			rebuild := protocol.RebuildProjectionEnd(builder)
			return finishRequest(builder, requestID,
				protocol.RequestPayloadRebuildProjection, rebuild)
		}, protocol.ResponsePayloadRebuildResult)
	if err != nil {
		return 0, err
	}
	var result protocol.RebuildResult
	result.Init(table.Bytes, table.Pos)
	return result.AppliedLsn(), nil
}
