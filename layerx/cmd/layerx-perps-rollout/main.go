package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/paxlabs-inc/layerx/internal/store"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usage()
	}
	uri := strings.TrimSpace(os.Getenv("LAYERX_POSTGRES_URI"))
	if uri == "" {
		return errors.New("LAYERX_POSTGRES_URI is required")
	}
	st, err := store.New(ctx, uri)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "status":
		state, err := st.GetPerpRolloutState(ctx)
		if err != nil {
			return err
		}
		report, err := st.PerpShadowReport(ctx)
		if err != nil {
			return err
		}
		drills, err := st.PerpFailureDrills(ctx)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"state": state, "shadow": report, "drills": drills}, nil)
	case "shadow-report":
		report, err := st.PerpShadowReport(ctx)
		if err != nil {
			return err
		}
		coverage, err := st.PerpShadowCoverage(ctx)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"intent_count": report.IntentCount, "first_intent_at": report.FirstIntentAt,
			"last_intent_at": report.LastIntentAt, "continuous_seconds": int64(report.ContinuousFor.Seconds()),
			"coverage_pairs": report.CoveragePairs, "mismatch_count": report.MismatchCount,
			"feed_gap_count": report.FeedGapCount, "economic_drift_rows": report.EconomicDriftRows,
			"gate_satisfied": report.GateSatisfied(), "coverage": coverage,
		}, nil)
	case "shadow-mismatches":
		fs := flag.NewFlagSet("shadow-mismatches", flag.ContinueOnError)
		limit := fs.Int("limit", 1000, "maximum rows")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		rows, err := st.PerpShadowMismatches(ctx, *limit)
		return printJSON(rows, err)
	case "advance":
		fs := flag.NewFlagSet("advance", flag.ContinueOnError)
		stage := fs.String("stage", "", "next stage")
		percent := fs.Int("percent", 0, "next traffic percentage")
		agents := fs.Bool("agents", false, "enable delegated agents")
		actor := fs.String("actor", "", "operator identity")
		reason := fs.String("reason", "", "auditable transition reason")
		evidencePath := fs.String("evidence", "", "JSON transition evidence file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		evidence := map[string]any{}
		if strings.TrimSpace(*evidencePath) != "" {
			if err := readJSONFile(*evidencePath, &evidence); err != nil {
				return err
			}
		}
		state, err := st.AdvancePerpRollout(ctx, strings.ToUpper(*stage), *percent,
			*agents, *actor, *reason, evidence)
		return printJSON(state, err)
	case "rollback":
		fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
		stage := fs.String("stage", "OFF", "OFF or SHADOW")
		actor := fs.String("actor", "", "operator identity")
		reason := fs.String("reason", "", "incident reason")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		state, err := st.RollbackPerpRollout(ctx, strings.ToUpper(*stage), *actor, *reason)
		return printJSON(state, err)
	case "record-gate":
		return recordGate(ctx, st, args[1:])
	case "drill":
		return recordDrill(ctx, st, args[1:])
	case "market-mode":
		return setMarketMode(ctx, st, args[1:])
	case "legacy-cutover":
		return recordLegacyCutover(ctx, st, args[1:])
	case "legacy-zero":
		return recordLegacyZero(ctx, st, args[1:])
	case "legacy-retire":
		return recordLegacyRetirement(ctx, st, args[1:])
	default:
		return usage()
	}
}

func setMarketMode(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("market-mode", flag.ContinueOnError)
	symbol := fs.String("symbol", "", "market symbol or ALL")
	nextMode := fs.String("mode", "", "OFF, SHADOW, CANARY, ACTIVE, REDUCE_ONLY, or PAUSED")
	cause := fs.String("cause", "", "pause or transition cause")
	actor := fs.String("actor", "", "operator identity")
	clearPause := fs.Bool("clear-pause", false, "explicitly clear a held PAUSED state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sym := strings.ToUpper(strings.TrimSpace(*symbol))
	next := strings.ToUpper(strings.TrimSpace(*nextMode))
	if sym == "ALL" {
		rows, err := st.SetAllPerpMarketModes(ctx, next, *cause, *actor, *clearPause)
		return printJSON(rows, err)
	}
	row, err := st.SetPerpMarketMode(ctx, sym, next, *cause, *actor, *clearPause)
	return printJSON(row, err)
}

func recordGate(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("record-gate", flag.ContinueOnError)
	accounting := fs.Int64("accounting-drift-usdx", 0, "signed accounting drift in micro-USDX")
	duplicates := fs.Int64("duplicate-fill-count", 0, "duplicate fill count")
	reconcile := fs.Int64("reconciliation-drift-usdx", 0, "signed reconciliation drift in micro-USDX")
	maxFeedAge := fs.Int64("max-feed-age-ms", 0, "maximum observed feed age")
	feed := fs.Bool("feed-fresh", false, "feed freshness gate is green")
	liquidation := fs.Int64("liquidation-p99-ms", 0, "liquidation p99 latency")
	insuranceCapital := fs.Int64("insurance-capital-usdx", 0, "insurance capital in micro-USDX")
	insuranceFloor := fs.Int64("insurance-floor-usdx", 0, "required insurance floor in micro-USDX")
	replayMissing := fs.Int64("private-replay-missing", 0, "missing private replay event count")
	manual := fs.Bool("manual-trading-stable", false, "manual trading window is stable")
	evidencePath := fs.String("evidence", "", "JSON evidence file")
	actor := fs.String("actor", "", "operator identity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	evidence := map[string]any{}
	if err := readJSONFile(*evidencePath, &evidence); err != nil {
		return err
	}
	return st.RecordPerpRolloutGate(ctx, store.PerpRolloutGateSample{
		AccountingDriftUSDX: *accounting, DuplicateFillCount: *duplicates,
		ReconciliationDriftUSDX: *reconcile, MaxFeedAgeMs: *maxFeedAge,
		FeedFresh: *feed, LiquidationP99Ms: *liquidation,
		InsuranceCapitalUSDX: *insuranceCapital, InsuranceFloorUSDX: *insuranceFloor,
		PrivateReplayMissing: *replayMissing, ManualTradingStable: *manual,
		Details: evidence, RecordedBy: *actor,
	})
}

func recordDrill(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("drill", flag.ContinueOnError)
	name := fs.String("name", "", "drill name")
	status := fs.String("status", "", "PENDING, RUNNING, PASSED, or FAILED")
	evidencePath := fs.String("evidence", "", "JSON evidence file")
	actor := fs.String("actor", "", "operator identity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	evidence := map[string]any{}
	if err := readJSONFile(*evidencePath, &evidence); err != nil {
		return err
	}
	return st.SetPerpFailureDrill(ctx, *name, strings.ToUpper(*status), *actor, evidence)
}

type legacyCutoverManifest struct {
	CutoverBlock             int64    `json:"cutover_block"`
	BlockHash                string   `json:"block_hash"`
	IndexerBlock             int64    `json:"indexer_block"`
	IndexerBlockHash         string   `json:"indexer_block_hash"`
	DiamondAddress           string   `json:"diamond_address"`
	SnapshotURI              string   `json:"snapshot_uri"`
	SnapshotSHA256           string   `json:"snapshot_sha256"`
	IndexerReconciled        bool     `json:"indexer_reconciled"`
	EntryOrdersCancelled     bool     `json:"entry_orders_cancelled"`
	ContractCloseOnly        bool     `json:"contract_close_only"`
	CloseOnlyTxHash          string   `json:"close_only_tx_hash"`
	CloseOnlyProofURI        string   `json:"close_only_proof_uri"`
	CancellationTxHashes     []string `json:"cancellation_tx_hashes"`
	Positions                int64    `json:"positions"`
	Orders                   int64    `json:"orders"`
	LockedCollateralUSDX     string   `json:"locked_collateral_usdx"`
	UnsettledFundingUSDX     string   `json:"unsettled_funding_usdx"`
	OwnerApprovedBy          string   `json:"owner_approved_by"`
	OwnerAuthorizationURI    string   `json:"owner_authorization_uri"`
	OwnerAuthorizationSHA256 string   `json:"owner_authorization_sha256"`
}

func recordLegacyCutover(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("legacy-cutover", flag.ContinueOnError)
	path := fs.String("manifest", "", "cutover evidence JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var m legacyCutoverManifest
	if err := readJSONFile(*path, &m); err != nil {
		return err
	}
	return st.RecordPerpLegacyCutover(ctx, store.PerpLegacyCutover{
		CutoverBlock: m.CutoverBlock, BlockHash: m.BlockHash,
		IndexerBlock: m.IndexerBlock, IndexerBlockHash: m.IndexerBlockHash,
		DiamondAddress: m.DiamondAddress,
		SnapshotURI:    m.SnapshotURI, SnapshotSHA256: m.SnapshotSHA256,
		IndexerReconciled: m.IndexerReconciled, EntryOrdersCancelled: m.EntryOrdersCancelled,
		ContractCloseOnly: m.ContractCloseOnly, CloseOnlyTxHash: m.CloseOnlyTxHash,
		CloseOnlyProofURI: m.CloseOnlyProofURI, CancellationTxHashes: m.CancellationTxHashes,
		Positions: m.Positions, Orders: m.Orders,
		LockedCollateralUSDX: m.LockedCollateralUSDX, UnsettledFundingUSDX: m.UnsettledFundingUSDX,
		OwnerApprovedBy: m.OwnerApprovedBy, OwnerAuthorizationURI: m.OwnerAuthorizationURI,
		OwnerAuthorizationSHA256: m.OwnerAuthorizationSHA256,
	})
}

type legacyZeroManifest struct {
	Positions             int64  `json:"positions"`
	Orders                int64  `json:"orders"`
	LockedCollateralUSDX  string `json:"locked_collateral_usdx"`
	UnsettledFundingUSDX  string `json:"unsettled_funding_usdx"`
	HistoryAvailableSince string `json:"history_available_since"`
	SourceURI             string `json:"source_uri"`
	SourceSHA256          string `json:"source_sha256"`
	ObservedBy            string `json:"observed_by"`
}

func recordLegacyZero(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("legacy-zero", flag.ContinueOnError)
	path := fs.String("manifest", "", "legacy zero-state evidence JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var m legacyZeroManifest
	if err := readJSONFile(*path, &m); err != nil {
		return err
	}
	var since *time.Time
	if m.HistoryAvailableSince != "" {
		t, err := time.Parse(time.RFC3339, m.HistoryAvailableSince)
		if err != nil {
			return fmt.Errorf("history_available_since: %w", err)
		}
		since = &t
	}
	return st.RecordPerpLegacyZeroCheck(ctx, store.PerpLegacyZeroCheck{
		Positions: m.Positions, Orders: m.Orders,
		LockedCollateralUSDX:  m.LockedCollateralUSDX,
		UnsettledFundingUSDX:  m.UnsettledFundingUSDX,
		HistoryAvailableSince: since, SourceURI: m.SourceURI,
		SourceSHA256: m.SourceSHA256, ObservedBy: m.ObservedBy,
	})
}

type legacyRetirementManifest struct {
	RetireTxHash             string `json:"retire_tx_hash"`
	ProofURI                 string `json:"proof_uri"`
	OwnerApprovedBy          string `json:"owner_approved_by"`
	OwnerAuthorizationURI    string `json:"owner_authorization_uri"`
	OwnerAuthorizationSHA256 string `json:"owner_authorization_sha256"`
}

func recordLegacyRetirement(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("legacy-retire", flag.ContinueOnError)
	path := fs.String("manifest", "", "legacy retirement proof JSON file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var m legacyRetirementManifest
	if err := readJSONFile(*path, &m); err != nil {
		return err
	}
	return st.RecordPerpLegacyRetirement(ctx, store.PerpLegacyRetirement{
		RetireTxHash: m.RetireTxHash, ProofURI: m.ProofURI,
		OwnerApprovedBy:          m.OwnerApprovedBy,
		OwnerAuthorizationURI:    m.OwnerAuthorizationURI,
		OwnerAuthorizationSHA256: m.OwnerAuthorizationSHA256,
	})
}

func readJSONFile(path string, dst any) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("evidence manifest path is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func printJSON(v any, err error) error {
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func usage() error {
	return errors.New("usage: layerx-perps-rollout <status|shadow-report|shadow-mismatches|advance|rollback|record-gate|drill|market-mode|legacy-cutover|legacy-zero|legacy-retire>; percentage values: " +
		strings.Join([]string{strconv.Itoa(1), strconv.Itoa(5), strconv.Itoa(25), strconv.Itoa(50), strconv.Itoa(100)}, ","))
}
