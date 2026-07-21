package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRolloutGate = errors.New("store: rollout gate is not satisfied")

type PerpShadowReport struct {
	IntentCount       int64
	FirstIntentAt     time.Time
	LastIntentAt      time.Time
	ContinuousFor     time.Duration
	CoveragePairs     int64
	MismatchCount     int64
	FeedGapCount      int64
	EconomicDriftRows int64
}

type PerpShadowCoverage struct {
	MarketSymbol string
	OrderType    string
	IntentCount  int64
	Mismatches   int64
	FeedGaps     int64
}

func (s *Store) PerpShadowCoverage(ctx context.Context) ([]PerpShadowCoverage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT market_symbol, order_type, count(*),
		       count(*) FILTER (WHERE NOT matched),
		       count(*) FILTER (WHERE feed_gap_detected)
		FROM perp_shadow_observations
		GROUP BY market_symbol, order_type
		ORDER BY market_symbol, order_type`)
	if err != nil {
		return nil, fmt.Errorf("store: shadow coverage: %w", err)
	}
	defer rows.Close()
	var out []PerpShadowCoverage
	for rows.Next() {
		var item PerpShadowCoverage
		if err := rows.Scan(&item.MarketSymbol, &item.OrderType, &item.IntentCount,
			&item.Mismatches, &item.FeedGaps); err != nil {
			return nil, fmt.Errorf("store: scan shadow coverage: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type PerpShadowMismatch struct {
	ID                             int64
	OrderID                        string
	OwnerDID                       string
	ActingDID                      string
	MarketSymbol                   string
	OrderType                      string
	Side                           string
	Contracts                      int64
	SnapshotID                     string
	OrderbookSeq                   int64
	StatsSeq                       int64
	SourceTimestampMs              int64
	EngineExecutionPriceCents      int64
	ReferenceExecutionPriceCents   int64
	EngineMarginUSDX               int64
	ReferenceMarginUSDX            int64
	EngineFeeUSDX                  int64
	ReferenceFeeUSDX               int64
	EngineFundingUSDX              int64
	ReferenceFundingUSDX           int64
	EngineLiquidationPriceCents    int64
	ReferenceLiquidationPriceCents int64
	EnginePnLUSDX                  int64
	ReferencePnLUSDX               int64
	EngineError                    string
	ReferenceError                 string
	EngineErrorCode                string
	ReferenceErrorCode             string
	ExecutionToleranceCents        int64
	MismatchFields                 []string
	FeedGapDetected                bool
	CreatedAt                      time.Time
}

func (s *Store) PerpShadowMismatches(ctx context.Context, limit int) ([]PerpShadowMismatch, error) {
	if limit <= 0 || limit > 10_000 {
		limit = 1_000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id,order_id::text,owner_did,acting_did,market_symbol,order_type,side,contracts,
		       snapshot_id,orderbook_seq,stats_seq,source_timestamp_ms,
		       engine_execution_price_cents,reference_execution_price_cents,
		       engine_margin_usdx,reference_margin_usdx,engine_fee_usdx,reference_fee_usdx,
		       engine_funding_usdx,reference_funding_usdx,
		       engine_liquidation_price_cents,reference_liquidation_price_cents,
		       engine_pnl_usdx,reference_pnl_usdx,engine_error,reference_error,
		       engine_error_code,reference_error_code,
		       execution_tolerance_cents,mismatch_fields,feed_gap_detected,created_at
		FROM perp_shadow_observations
		WHERE NOT matched OR feed_gap_detected
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: shadow mismatches: %w", err)
	}
	defer rows.Close()
	var out []PerpShadowMismatch
	for rows.Next() {
		var item PerpShadowMismatch
		if err := rows.Scan(
			&item.ID, &item.OrderID, &item.OwnerDID, &item.ActingDID,
			&item.MarketSymbol, &item.OrderType, &item.Side, &item.Contracts,
			&item.SnapshotID, &item.OrderbookSeq, &item.StatsSeq, &item.SourceTimestampMs,
			&item.EngineExecutionPriceCents, &item.ReferenceExecutionPriceCents,
			&item.EngineMarginUSDX, &item.ReferenceMarginUSDX,
			&item.EngineFeeUSDX, &item.ReferenceFeeUSDX,
			&item.EngineFundingUSDX, &item.ReferenceFundingUSDX,
			&item.EngineLiquidationPriceCents, &item.ReferenceLiquidationPriceCents,
			&item.EnginePnLUSDX, &item.ReferencePnLUSDX,
			&item.EngineError, &item.ReferenceError,
			&item.EngineErrorCode, &item.ReferenceErrorCode,
			&item.ExecutionToleranceCents,
			&item.MismatchFields, &item.FeedGapDetected, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan shadow mismatch: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r PerpShadowReport) GateSatisfied() bool {
	return r.IntentCount >= 100_000 &&
		r.ContinuousFor >= 7*24*time.Hour &&
		r.CoveragePairs == 18*6 &&
		r.MismatchCount == 0 &&
		r.FeedGapCount == 0 &&
		r.EconomicDriftRows == 0
}

func (s *Store) PerpShadowReport(ctx context.Context) (PerpShadowReport, error) {
	var report PerpShadowReport
	var first, last *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at), max(created_at),
		       count(DISTINCT (market_symbol, order_type)),
		       count(*) FILTER (WHERE NOT matched),
		       count(*) FILTER (WHERE feed_gap_detected),
		       count(*) FILTER (WHERE account_balance_before_usdx <> account_balance_after_usdx
		         OR position_count_before <> position_count_after
		         OR fill_count_before <> fill_count_after)
		FROM perp_shadow_observations`).
		Scan(&report.IntentCount, &first, &last, &report.CoveragePairs,
			&report.MismatchCount, &report.FeedGapCount, &report.EconomicDriftRows)
	if err != nil {
		return PerpShadowReport{}, fmt.Errorf("store: shadow report: %w", err)
	}
	if first != nil {
		report.FirstIntentAt = *first
	}
	if last != nil {
		report.LastIntentAt = *last
	}
	if first != nil && last != nil {
		report.ContinuousFor = last.Sub(*first)
	}
	return report, nil
}

type PerpRolloutState struct {
	Stage                string
	TrafficPercent       int
	AgentsEnabled        bool
	LegacyCutoverBlock   int64
	LegacyCloseOnly      bool
	DiamondWritesRetired bool
	ChangedBy            string
	ChangeReason         string
	UpdatedAt            time.Time
}

func scanPerpRolloutState(row pgx.Row) (PerpRolloutState, error) {
	var state PerpRolloutState
	var cutover *int64
	err := row.Scan(&state.Stage, &state.TrafficPercent, &state.AgentsEnabled,
		&cutover, &state.LegacyCloseOnly, &state.DiamondWritesRetired,
		&state.ChangedBy, &state.ChangeReason, &state.UpdatedAt)
	if cutover != nil {
		state.LegacyCutoverBlock = *cutover
	}
	return state, err
}

func (s *Store) GetPerpRolloutState(ctx context.Context) (PerpRolloutState, error) {
	state, err := scanPerpRolloutState(s.pool.QueryRow(ctx, `
		SELECT stage, traffic_percent, agents_enabled, legacy_cutover_block,
		       legacy_close_only, diamond_writes_retired, changed_by, change_reason, updated_at
		FROM perp_rollout_state WHERE singleton`))
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: rollout state: %w", err)
	}
	return state, nil
}

type PerpRolloutGateSample struct {
	AccountingDriftUSDX     int64
	DuplicateFillCount      int64
	ReconciliationDriftUSDX int64
	MaxFeedAgeMs            int64
	LiquidationP99Ms        int64
	InsuranceCapitalUSDX    int64
	InsuranceFloorUSDX      int64
	PrivateReplayMissing    int64
	AccountingExact         bool
	DuplicateFillsZero      bool
	ReconciliationClean     bool
	FeedFresh               bool
	LiquidationLatencyGreen bool
	InsuranceAboveFloor     bool
	PrivateReplayComplete   bool
	ManualTradingStable     bool
	Details                 map[string]any
	RecordedBy              string
	RecordedAt              time.Time
}

func (g PerpRolloutGateSample) Green() bool {
	return g.AccountingExact && g.DuplicateFillsZero && g.ReconciliationClean &&
		g.FeedFresh && g.LiquidationLatencyGreen && g.InsuranceAboveFloor &&
		g.PrivateReplayComplete
}

func (s *Store) RecordPerpRolloutGate(ctx context.Context, sample PerpRolloutGateSample) error {
	if sample.RecordedBy == "" {
		return fmt.Errorf("store: rollout gate requires recorder")
	}
	if sample.DuplicateFillCount < 0 || sample.MaxFeedAgeMs < 0 ||
		sample.LiquidationP99Ms <= 0 || sample.InsuranceCapitalUSDX < 0 ||
		sample.InsuranceFloorUSDX <= 0 || sample.PrivateReplayMissing < 0 {
		return fmt.Errorf("store: rollout gate metrics are invalid or missing")
	}
	sample.AccountingExact = sample.AccountingDriftUSDX == 0
	sample.DuplicateFillsZero = sample.DuplicateFillCount == 0
	sample.ReconciliationClean = sample.ReconciliationDriftUSDX == 0
	sample.LiquidationLatencyGreen = sample.LiquidationP99Ms <= 1_000
	sample.InsuranceAboveFloor = sample.InsuranceCapitalUSDX >= sample.InsuranceFloorUSDX
	sample.PrivateReplayComplete = sample.PrivateReplayMissing == 0
	if sample.Green() {
		if err := requireEvidence(sample.Details, "artifact_uri", "artifact_sha256", "window_started_at", "window_ended_at"); err != nil {
			return fmt.Errorf("store: rollout gate evidence: %w", err)
		}
	}
	details, err := json.Marshal(sample.Details)
	if err != nil {
		return fmt.Errorf("store: encode rollout gate details: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO perp_rollout_gate_samples (
		  accounting_drift_usdx,duplicate_fill_count,reconciliation_drift_usdx,
		  max_feed_age_ms,liquidation_p99_ms,insurance_capital_usdx,
		  insurance_floor_usdx,private_replay_missing,feed_fresh,
		  manual_trading_stable,details,recorded_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		sample.AccountingDriftUSDX, sample.DuplicateFillCount,
		sample.ReconciliationDriftUSDX, sample.MaxFeedAgeMs,
		sample.LiquidationP99Ms, sample.InsuranceCapitalUSDX,
		sample.InsuranceFloorUSDX, sample.PrivateReplayMissing,
		sample.FeedFresh, sample.ManualTradingStable, details, sample.RecordedBy)
	if err != nil {
		return fmt.Errorf("store: record rollout gate: %w", err)
	}
	return nil
}

func readLatestGate(ctx context.Context, tx pgx.Tx) (PerpRolloutGateSample, error) {
	var sample PerpRolloutGateSample
	err := tx.QueryRow(ctx, `
		SELECT accounting_exact, duplicate_fills_zero, reconciliation_clean, feed_fresh,
		       liquidation_latency_green, insurance_above_floor, private_replay_complete,
		       manual_trading_stable, recorded_by, recorded_at
		FROM perp_rollout_gate_samples ORDER BY recorded_at DESC LIMIT 1`).
		Scan(&sample.AccountingExact, &sample.DuplicateFillsZero, &sample.ReconciliationClean,
			&sample.FeedFresh, &sample.LiquidationLatencyGreen, &sample.InsuranceAboveFloor,
			&sample.PrivateReplayComplete, &sample.ManualTradingStable,
			&sample.RecordedBy, &sample.RecordedAt)
	return sample, err
}

func validRolloutTransition(from PerpRolloutState, toStage string, toPercent int) bool {
	switch from.Stage {
	case "OFF":
		return toStage == "SHADOW" && toPercent == 0
	case "SHADOW":
		return toStage == "STAFF" && toPercent == 0
	case "STAFF":
		return toStage == "PERCENT" && toPercent == 1
	case "PERCENT":
		next := map[int]int{1: 5, 5: 25, 25: 50, 50: 100}
		if expected, ok := next[from.TrafficPercent]; ok {
			return toStage == "PERCENT" && toPercent == expected
		}
		return from.TrafficPercent == 100 && toStage == "FULL" && toPercent == 100
	case "FULL":
		return toStage == "RETIRE_READY" && toPercent == 100
	case "RETIRE_READY":
		return toStage == "RETIRED" && toPercent == 100
	default:
		return false
	}
}

func (s *Store) AdvancePerpRollout(ctx context.Context, toStage string, toPercent int,
	agentsEnabled bool, changedBy, reason string, evidence map[string]any) (PerpRolloutState, error) {
	if changedBy == "" || reason == "" {
		return PerpRolloutState{}, fmt.Errorf("store: rollout transition requires actor and reason")
	}
	rawEvidence, err := json.Marshal(evidence)
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: encode rollout transition evidence: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: begin rollout transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanPerpRolloutState(tx.QueryRow(ctx, `
		SELECT stage, traffic_percent, agents_enabled, legacy_cutover_block,
		       legacy_close_only, diamond_writes_retired, changed_by, change_reason, updated_at
		FROM perp_rollout_state WHERE singleton FOR UPDATE`))
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: lock rollout state: %w", err)
	}
	if !validRolloutTransition(current, toStage, toPercent) {
		return PerpRolloutState{}, fmt.Errorf("%w: %s/%d -> %s/%d", ErrRolloutGate,
			current.Stage, current.TrafficPercent, toStage, toPercent)
	}
	if current.Stage == "SHADOW" {
		if err := requireEvidence(evidence, "owner_authorization_uri", "owner_authorization_sha256"); err != nil {
			return PerpRolloutState{}, fmt.Errorf("store: canary authorization evidence: %w", err)
		}
		var count, pairs, mismatches, gaps, drift int64
		var first, last *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at), max(created_at),
			       count(DISTINCT (market_symbol, order_type)),
			       count(*) FILTER (WHERE NOT matched),
			       count(*) FILTER (WHERE feed_gap_detected),
			       count(*) FILTER (WHERE account_balance_before_usdx <> account_balance_after_usdx
			         OR position_count_before <> position_count_after OR fill_count_before <> fill_count_after)
			FROM perp_shadow_observations`).
			Scan(&count, &first, &last, &pairs, &mismatches, &gaps, &drift); err != nil {
			return PerpRolloutState{}, fmt.Errorf("store: read shadow transition gate: %w", err)
		}
		if first == nil || last == nil || count < 100_000 || last.Sub(*first) < 7*24*time.Hour ||
			pairs != 18*6 || mismatches != 0 || gaps != 0 || drift != 0 {
			return PerpRolloutState{}, ErrRolloutGate
		}
		var liquidity, insurance int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(capital_usdx) FILTER (WHERE id='liquidity'),0),
			       COALESCE(SUM(capital_usdx) FILTER (WHERE id='insurance'),0)
			FROM perp_pools`).Scan(&liquidity, &insurance); err != nil {
			return PerpRolloutState{}, err
		}
		if liquidity <= 0 || insurance <= 0 {
			return PerpRolloutState{}, ErrRolloutGate
		}
	}
	if current.Stage == "STAFF" {
		var pending int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM perp_failure_drills WHERE status <> 'PASSED'`).Scan(&pending); err != nil {
			return PerpRolloutState{}, err
		}
		if pending != 0 {
			return PerpRolloutState{}, ErrRolloutGate
		}
	}
	if current.Stage == "STAFF" || current.Stage == "PERCENT" || current.Stage == "FULL" {
		gate, err := readLatestGate(ctx, tx)
		if err != nil || !gate.Green() {
			return PerpRolloutState{}, ErrRolloutGate
		}
		if !gate.RecordedAt.After(current.UpdatedAt) {
			return PerpRolloutState{}, ErrRolloutGate
		}
		if agentsEnabled && !gate.ManualTradingStable {
			return PerpRolloutState{}, ErrRolloutGate
		}
	}
	if agentsEnabled && toStage != "PERCENT" && toStage != "FULL" &&
		toStage != "RETIRE_READY" && toStage != "RETIRED" {
		return PerpRolloutState{}, ErrRolloutGate
	}
	if current.Stage == "STAFF" && toStage == "PERCENT" &&
		(!current.LegacyCloseOnly || current.LegacyCutoverBlock <= 0) {
		return PerpRolloutState{}, ErrRolloutGate
	}
	if toStage == "RETIRE_READY" || toStage == "RETIRED" {
		var positions, orders int64
		var locked, funding string
		var allZero bool
		var historySince *time.Time
		err := tx.QueryRow(ctx, `
			SELECT positions, orders, locked_collateral_usdx::text, unsettled_funding_usdx::text,
			       all_zero, history_available_since
			FROM perp_legacy_zero_checks ORDER BY observed_at DESC LIMIT 1`).
			Scan(&positions, &orders, &locked, &funding, &allZero, &historySince)
		if err != nil || !allZero || historySince == nil || time.Since(*historySince) < 30*24*time.Hour {
			return PerpRolloutState{}, ErrRolloutGate
		}
		_ = positions
		_ = orders
		_ = locked
		_ = funding
		if !current.LegacyCloseOnly {
			return PerpRolloutState{}, ErrRolloutGate
		}
	}
	if toStage == "RETIRED" && !current.DiamondWritesRetired {
		return PerpRolloutState{}, ErrRolloutGate
	}
	retired := current.DiamondWritesRetired
	next, err := scanPerpRolloutState(tx.QueryRow(ctx, `
		UPDATE perp_rollout_state
		SET stage=$1, traffic_percent=$2, agents_enabled=$3,
		    diamond_writes_retired=$4, changed_by=$5, change_reason=$6, updated_at=now()
		WHERE singleton
		RETURNING stage, traffic_percent, agents_enabled, legacy_cutover_block,
		          legacy_close_only, diamond_writes_retired, changed_by, change_reason, updated_at`,
		toStage, toPercent, agentsEnabled, retired, changedBy, reason))
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: update rollout state: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO perp_rollout_changes (
		  from_stage,to_stage,from_percent,to_percent,agents_enabled,legacy_cutover_block,
		  legacy_close_only,diamond_writes_retired,changed_by,reason,evidence)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6::bigint,0),$7,$8,$9,$10,$11)`,
		current.Stage, next.Stage, current.TrafficPercent, next.TrafficPercent,
		next.AgentsEnabled, next.LegacyCutoverBlock, next.LegacyCloseOnly,
		next.DiamondWritesRetired, changedBy, reason, rawEvidence); err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: journal rollout transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: commit rollout transition: %w", err)
	}
	return next, nil
}

// RollbackPerpRollout immediately closes ACTIVE risk-increase admission while
// preserving the legacy cutover evidence. Reduction/cancellation remain
// available through their normal mode permissions. A retired Diamond cannot
// be resurrected by this control.
func (s *Store) RollbackPerpRollout(ctx context.Context, toStage, changedBy, reason string) (PerpRolloutState, error) {
	if (toStage != "OFF" && toStage != "SHADOW") || changedBy == "" || reason == "" {
		return PerpRolloutState{}, fmt.Errorf("store: rollback requires OFF or SHADOW, actor, and reason")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: begin rollout rollback: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanPerpRolloutState(tx.QueryRow(ctx, `
		SELECT stage, traffic_percent, agents_enabled, legacy_cutover_block,
		       legacy_close_only, diamond_writes_retired, changed_by, change_reason, updated_at
		FROM perp_rollout_state WHERE singleton FOR UPDATE`))
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: lock rollout state: %w", err)
	}
	if current.Stage == "RETIRED" || current.DiamondWritesRetired {
		return PerpRolloutState{}, fmt.Errorf("%w: retired legacy writes cannot be restored", ErrRolloutGate)
	}
	next, err := scanPerpRolloutState(tx.QueryRow(ctx, `
		UPDATE perp_rollout_state
		SET stage=$1, traffic_percent=0, agents_enabled=FALSE,
		    changed_by=$2, change_reason=$3, updated_at=now()
		WHERE singleton
		RETURNING stage, traffic_percent, agents_enabled, legacy_cutover_block,
		          legacy_close_only, diamond_writes_retired, changed_by, change_reason, updated_at`,
		toStage, changedBy, reason))
	if err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: apply rollout rollback: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO perp_rollout_changes (
		  from_stage,to_stage,from_percent,to_percent,agents_enabled,legacy_cutover_block,
		  legacy_close_only,diamond_writes_retired,changed_by,reason,evidence)
		VALUES ($1,$2,$3,$4,FALSE,NULLIF($5::bigint,0),$6,FALSE,$7,$8,'{}'::jsonb)`,
		current.Stage, next.Stage, current.TrafficPercent, next.TrafficPercent,
		next.LegacyCutoverBlock, next.LegacyCloseOnly, changedBy, reason); err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: journal rollout rollback: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PerpRolloutState{}, fmt.Errorf("store: commit rollout rollback: %w", err)
	}
	return next, nil
}

func (s *Store) SetPerpFailureDrill(ctx context.Context, name, status, changedBy string,
	evidence map[string]any) error {
	if changedBy == "" {
		return fmt.Errorf("store: failure drill requires actor")
	}
	if status != "PENDING" && status != "RUNNING" && status != "PASSED" && status != "FAILED" {
		return fmt.Errorf("store: failure drill status %q is invalid", status)
	}
	if status == "PASSED" {
		if err := requireEvidence(evidence, "artifact_uri", "artifact_sha256", "observed_result"); err != nil {
			return fmt.Errorf("store: failure drill evidence: %w", err)
		}
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("store: encode drill evidence: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE perp_failure_drills
		SET status=$2,
		    started_at=CASE WHEN $2='RUNNING' THEN COALESCE(started_at,now()) ELSE started_at END,
		    completed_at=CASE WHEN $2 IN ('PASSED','FAILED') THEN now() ELSE NULL END,
		    evidence=$3, changed_by=$4, updated_at=now()
		WHERE name=$1`, name, status, raw, changedBy)
	if err != nil {
		return fmt.Errorf("store: update failure drill: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

type PerpFailureDrill struct {
	Name        string
	Status      string
	StartedAt   *time.Time
	CompletedAt *time.Time
	Evidence    map[string]any
	ChangedBy   string
	UpdatedAt   time.Time
}

func (s *Store) PerpFailureDrills(ctx context.Context) ([]PerpFailureDrill, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name,status,started_at,completed_at,evidence,changed_by,updated_at
		FROM perp_failure_drills ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: failure drills: %w", err)
	}
	defer rows.Close()
	var out []PerpFailureDrill
	for rows.Next() {
		var item PerpFailureDrill
		var raw []byte
		if err := rows.Scan(&item.Name, &item.Status, &item.StartedAt,
			&item.CompletedAt, &raw, &item.ChangedBy, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan failure drill: %w", err)
		}
		if err := json.Unmarshal(raw, &item.Evidence); err != nil {
			return nil, fmt.Errorf("store: decode failure drill evidence: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func requireEvidence(evidence map[string]any, fields ...string) error {
	for _, field := range fields {
		value, ok := evidence[field].(string)
		if !ok || value == "" {
			return fmt.Errorf("%s is required", field)
		}
		if strings.HasSuffix(field, "_sha256") {
			raw, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
			if err != nil || len(raw) != 32 {
				return fmt.Errorf("%s must be 32-byte hex", field)
			}
		}
	}
	return nil
}

func validSHA256(value string) bool {
	value = strings.TrimPrefix(value, "0x")
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32
}

type PerpLegacyCutover struct {
	CutoverBlock             int64
	BlockHash                string
	IndexerBlock             int64
	IndexerBlockHash         string
	DiamondAddress           string
	SnapshotURI              string
	SnapshotSHA256           string
	IndexerReconciled        bool
	EntryOrdersCancelled     bool
	ContractCloseOnly        bool
	CloseOnlyTxHash          string
	CloseOnlyProofURI        string
	CancellationTxHashes     []string
	Positions                int64
	Orders                   int64
	LockedCollateralUSDX     string
	UnsettledFundingUSDX     string
	OwnerApprovedBy          string
	OwnerAuthorizationURI    string
	OwnerAuthorizationSHA256 string
}

func (s *Store) RecordPerpLegacyCutover(ctx context.Context, cutover PerpLegacyCutover) error {
	if cutover.CutoverBlock <= 0 || cutover.OwnerApprovedBy == "" ||
		cutover.IndexerBlock != cutover.CutoverBlock || cutover.Orders != 0 ||
		cutover.BlockHash == "" || cutover.IndexerBlockHash != cutover.BlockHash ||
		cutover.DiamondAddress == "" || cutover.SnapshotURI == "" ||
		!validSHA256(cutover.BlockHash) || !validSHA256(cutover.SnapshotSHA256) ||
		!validSHA256(cutover.CloseOnlyTxHash) ||
		cutover.CloseOnlyProofURI == "" || cutover.OwnerAuthorizationURI == "" ||
		!validSHA256(cutover.OwnerAuthorizationSHA256) || !cutover.IndexerReconciled ||
		!cutover.EntryOrdersCancelled || !cutover.ContractCloseOnly {
		return ErrRolloutGate
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var stage string
	if err := tx.QueryRow(ctx, `
		SELECT stage FROM perp_rollout_state WHERE singleton FOR UPDATE`).Scan(&stage); err != nil {
		return fmt.Errorf("store: lock rollout for legacy cutover: %w", err)
	}
	if stage != "STAFF" {
		return fmt.Errorf("%w: legacy cutover requires STAFF stage", ErrRolloutGate)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO perp_legacy_cutovers (
		  cutover_block,block_hash,indexer_block,indexer_block_hash,
		  diamond_address,snapshot_uri,snapshot_sha256,
		  indexer_reconciled,entry_orders_cancelled,contract_close_only,
		  close_only_tx_hash,close_only_proof_uri,cancellation_tx_hashes,
		  positions,orders,locked_collateral_usdx,unsettled_funding_usdx,owner_approved_by,
		  owner_authorization_uri,owner_authorization_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
		        $14,$15,$16::numeric,$17::numeric,$18,$19,$20)`,
		cutover.CutoverBlock, cutover.BlockHash, cutover.IndexerBlock,
		cutover.IndexerBlockHash, cutover.DiamondAddress,
		cutover.SnapshotURI, strings.TrimPrefix(cutover.SnapshotSHA256, "0x"), cutover.IndexerReconciled,
		cutover.EntryOrdersCancelled, cutover.ContractCloseOnly, cutover.CloseOnlyTxHash,
		cutover.CloseOnlyProofURI, cutover.CancellationTxHashes, cutover.Positions,
		cutover.Orders, cutover.LockedCollateralUSDX,
		cutover.UnsettledFundingUSDX, cutover.OwnerApprovedBy,
		cutover.OwnerAuthorizationURI,
		strings.TrimPrefix(cutover.OwnerAuthorizationSHA256, "0x")); err != nil {
		return fmt.Errorf("store: record legacy cutover: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_rollout_state
		SET legacy_cutover_block=$1, legacy_close_only=TRUE,
		    changed_by=$2, change_reason='owner-approved legacy cutover recorded', updated_at=now()
		WHERE singleton`, cutover.CutoverBlock, cutover.OwnerApprovedBy); err != nil {
		return fmt.Errorf("store: attach legacy cutover: %w", err)
	}
	return tx.Commit(ctx)
}

type PerpLegacyZeroCheck struct {
	Positions             int64
	Orders                int64
	LockedCollateralUSDX  string
	UnsettledFundingUSDX  string
	HistoryAvailableSince *time.Time
	SourceURI             string
	SourceSHA256          string
	ObservedBy            string
}

func (s *Store) RecordPerpLegacyZeroCheck(ctx context.Context, check PerpLegacyZeroCheck) error {
	if check.Positions < 0 || check.Orders < 0 || check.SourceURI == "" ||
		!validSHA256(check.SourceSHA256) || check.ObservedBy == "" {
		return ErrRolloutGate
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO perp_legacy_zero_checks (
		  positions,orders,locked_collateral_usdx,unsettled_funding_usdx,
		  history_available_since,source_uri,source_sha256,observed_by)
		VALUES ($1,$2,$3::numeric,$4::numeric,$5,$6,$7,$8)`,
		check.Positions, check.Orders, check.LockedCollateralUSDX,
		check.UnsettledFundingUSDX, check.HistoryAvailableSince,
		check.SourceURI, strings.TrimPrefix(check.SourceSHA256, "0x"), check.ObservedBy)
	if err != nil {
		return fmt.Errorf("store: record legacy zero check: %w", err)
	}
	return nil
}

type PerpLegacyRetirement struct {
	RetireTxHash             string
	ProofURI                 string
	OwnerApprovedBy          string
	OwnerAuthorizationURI    string
	OwnerAuthorizationSHA256 string
}

// RecordPerpLegacyRetirement records proof that the now-empty Diamond write
// surface was actually retired. It never marks RETIRED directly; the normal
// transition still requires a fresh zero/history check.
func (s *Store) RecordPerpLegacyRetirement(ctx context.Context, retirement PerpLegacyRetirement) error {
	if !validSHA256(retirement.RetireTxHash) || retirement.ProofURI == "" ||
		retirement.OwnerApprovedBy == "" || retirement.OwnerAuthorizationURI == "" ||
		!validSHA256(retirement.OwnerAuthorizationSHA256) {
		return ErrRolloutGate
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state PerpRolloutState
	state, err = scanPerpRolloutState(tx.QueryRow(ctx, `
		SELECT stage, traffic_percent, agents_enabled, legacy_cutover_block,
		       legacy_close_only, diamond_writes_retired, changed_by, change_reason, updated_at
		FROM perp_rollout_state WHERE singleton FOR UPDATE`))
	if err != nil {
		return err
	}
	if state.Stage != "RETIRE_READY" || !state.LegacyCloseOnly || state.DiamondWritesRetired {
		return ErrRolloutGate
	}
	var allZero bool
	var historySince *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT all_zero,history_available_since
		FROM perp_legacy_zero_checks ORDER BY observed_at DESC LIMIT 1`).
		Scan(&allZero, &historySince); err != nil || !allZero || historySince == nil ||
		time.Since(*historySince) < 30*24*time.Hour {
		return ErrRolloutGate
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO perp_legacy_retirements (
		  retire_tx_hash,proof_uri,owner_approved_by,
		  owner_authorization_uri,owner_authorization_sha256)
		VALUES ($1,$2,$3,$4,$5)`,
		retirement.RetireTxHash, retirement.ProofURI, retirement.OwnerApprovedBy,
		retirement.OwnerAuthorizationURI,
		strings.TrimPrefix(retirement.OwnerAuthorizationSHA256, "0x")); err != nil {
		return fmt.Errorf("store: record legacy retirement: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE perp_rollout_state
		SET diamond_writes_retired=TRUE,changed_by=$1,
		    change_reason='owner-approved empty Diamond write retirement proven',updated_at=now()
		WHERE singleton`, retirement.OwnerApprovedBy); err != nil {
		return fmt.Errorf("store: mark legacy writes retired: %w", err)
	}
	return tx.Commit(ctx)
}
