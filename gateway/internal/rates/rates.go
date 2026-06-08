// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package rates is the single source of truth for model -> PAX/Mtoken
// price points used by the credit_ledger. The table is versioned
// (RateTableVersion) so historical ledger rows remain auditable: every
// row records the rate_table_v that priced it, and operators can
// verify costs replay byte-identical even after the table is bumped.
//
// Cost formula:
//
//	cost_pax = (prompt_tokens * input_pax_per_mtoken
//	          + completion_tokens * output_pax_per_mtoken) / 1_000_000
//
// All math is decimal-string round-trip safe (we use math/big.Float
// for the divide so precision is preserved when the ledger writer
// stamps NUMERIC(20,12) into Postgres). PAX-per-Mtoken values are
// derived from the underlying USD provider prices at the launch
// reference 1 PAX = $11.43 (new_pax = usd_per_mtoken / 11.43). The
// `≈ $X` comments on each row are the USD invariant the PAX amounts
// are computed from; reprice = recompute PAX, bump RateTableVersion.
//
// Concurrency: all exported functions are pure (no shared state) and
// safe for concurrent use.
package rates

import (
	"fmt"
	"math/big"
	"strings"
)

// RateTableVersion identifies the immutable rate-card snapshot used to
// price LLM calls. Bump on every schedule change. Ledger rows record
// this so historical cost computations remain reproducible after a
// later rate update.
//
// v2 (2026-06-01, v1 launch): added deepseek-v4-pro (planner upgrade +
// compiler low-confidence escalation target) and kimi-k2.6 (executor
// upgrade). See FreeTierWhitelist + rateTable below.
//
// v3 (2026-06-04, PAX reference reprice): re-denominated the entire
// rate card from the placeholder 1 PAX = $0.01 anchor to the actual
// launch price 1 PAX = $11.43. No models added/removed and no USD
// targets changed — every PAX-per-Mtoken value was recomputed as
// usd_per_mtoken / 11.43 (a uniform /1143 vs v2). Also introduced the
// DailyFreeTierLimitPax cap (10 PAX/day). Ledger rows priced under v2
// keep their v2 figures; only v3+ rows use the new amounts.
//
// v4 (2026-06-08) adds glm-5p1-fast to the rate card for the new `neo`
// slot (Neo's cheap background model: write-back / compaction / summary
// validation). NOTE: the glm rate below is a PLACEHOLDER mirrored from the
// deepseek-v4-flash tier — Andrew to replace with the real provider price.
const RateTableVersion = 4

// PaxUsdReference is the USD price of 1 PAX the v3 rate card was
// denominated against. Exposed so ops/telemetry can re-derive or
// audit the PAX amounts from the row-level `≈ $X` USD targets.
const PaxUsdReference = 11.43

// freeTierGroup partitions models into the lanes the gateway routes
// on. Each lane has its own free-tier whitelist (see internal/routing).
//
// Group strings echo verbatim into the Prometheus labels for cost
// telemetry, so they MUST stay stable across versions.
const (
	GroupReason    = "reason"
	GroupClassify  = "classify"
	GroupSummarize = "summarize"
	GroupCode      = "code"
	GroupOther     = "other"
)

// DailyFreeTierLimitPax is the per-account free-tier spend cap, in PAX,
// enforced by internal/routing against the running daily total. It is a
// decimal string so it composes directly with AddPax/SubPax/CmpPax and
// the ledger's PAX representation. Callers that reach it must flip
// X-Matrix-BYO-API-Key to continue.
//
// 10 PAX/day = $114.30/day at the v3 reference (1 PAX = $11.43). Note
// this is a large headroom relative to per-call costs (e.g. ~127M
// deepseek-v4-flash output tokens/day); tighten here if the intent is a
// stricter free lane.
const DailyFreeTierLimitPax = "10"

// Free-tier model identifiers. v1 launch (2026-06-01) per-slot whitelist:
//
//	compiler — gpt-oss-120b (primary) + deepseek-v4-pro (low-confidence
//	           escalation target; compile.go re-invokes the compiler slot
//	           with the stronger model when a frame self-reports low
//	           confidence or an invalid verb).
//	planner  — gpt-oss-120b + deepseek-v4-flash + deepseek-v4-pro (the
//	           launch planner upgrade, pinned via MATRIX_PLANNER_MODEL).
//	executor — deepseek-v4-flash + kimi-k2.6 (the launch executor upgrade,
//	           pinned via MATRIX_EXECUTOR_MODEL).
//
// Documented in plan §5.15. Other slots/models are 403 unless the caller
// flips X-Matrix-BYO-API-Key.
//
// Constants are exported so internal/routing imports them rather than
// re-declaring; ALSO exposed via FreeTierWhitelist() for tests.
const (
	ModelCompilerFreeTier = "accounts/fireworks/models/gpt-oss-120b"
	ModelExecutorFreeTier = "accounts/fireworks/models/deepseek-v4-flash"
	ModelDeepSeekV4Flash  = "accounts/fireworks/models/deepseek-v4-flash"
	ModelDeepSeekV4Pro    = "accounts/fireworks/models/deepseek-v4-pro"
	ModelKimiK26          = "accounts/fireworks/routers/kimi-k2p6-fast"
	ModelGLM5p1Fast       = "accounts/fireworks/routers/glm-5p1-fast"
	ModelGPTOSS120B       = "accounts/fireworks/models/gpt-oss-120b"
	ModelGPTOSS20B        = "accounts/fireworks/models/gpt-oss-20b"
	ModelQwenCoder        = "Qwen/Qwen3-Coder-480B-A35B-Instruct-FP8"
	ModelLlama405B        = "meta-llama/Llama-3.1-405B-Instruct"
)

// Rate is the per-Mtoken price in PAX for a single model. Both prompt
// (input) and completion (output) sides are tracked because providers
// generally price the two asymmetrically.
type Rate struct {
	Model               string
	Group               string
	InputPaxPerMTokens  float64 // PAX charged per million prompt tokens
	OutputPaxPerMTokens float64 // PAX charged per million completion tokens
	Notes               string  // free-form provenance / sourcing
}

// rateTable is the immutable v3 rate card. Adding a model OR repricing
// REQUIRES bumping RateTableVersion to keep historical ledger rows
// replayable. Order is documentation only; the lookup is a map (see init).
//
// PAX values = USD target / 11.43 (1 PAX = $11.43, v3 reference). The
// `≈ $X` USD targets are the invariant; bake in real provider rates
// before GA and re-derive.
var rateTable = []Rate{
	{
		Model:               ModelGPTOSS120B,
		Group:               GroupReason,
		InputPaxPerMTokens:  0.052493438, // ≈ $0.60 / Mtoken
		OutputPaxPerMTokens: 0.104986877, // ≈ $1.20 / Mtoken
		Notes:               "Fireworks gpt-oss-120b free-tier compiler model",
	},
	{
		Model:               ModelGPTOSS20B,
		Group:               GroupClassify,
		InputPaxPerMTokens:  0.017497813, // ≈ $0.20 / Mtoken
		OutputPaxPerMTokens: 0.034995626, // ≈ $0.40 / Mtoken
		Notes:               "Fireworks gpt-oss-20b classifier/transform tier",
	},
	{
		Model:               ModelDeepSeekV4Flash,
		Group:               GroupSummarize,
		InputPaxPerMTokens:  0.026246719, // ≈ $0.30 / Mtoken (1M context)
		OutputPaxPerMTokens: 0.078740157, // ≈ $0.90 / Mtoken
		Notes:               "Fireworks deepseek-v4-flash free-tier executor / summarize",
	},
	{
		Model:               ModelQwenCoder,
		Group:               GroupCode,
		InputPaxPerMTokens:  0.078740157, // ≈ $0.90 / Mtoken
		OutputPaxPerMTokens: 0.157480315, // ≈ $1.80 / Mtoken
		Notes:               "Together Qwen3-Coder-480B code specialist",
	},
	{
		Model:               ModelLlama405B,
		Group:               GroupReason,
		InputPaxPerMTokens:  0.131233596, // ≈ $1.50 / Mtoken
		OutputPaxPerMTokens: 0.262467192, // ≈ $3.00 / Mtoken
		Notes:               "Together Llama-3.1-405B reasoning fallback",
	},
	{
		Model:               ModelDeepSeekV4Pro,
		Group:               GroupReason,
		InputPaxPerMTokens:  0.087489064, // ≈ $1.00 / Mtoken (frontier reasoning)
		OutputPaxPerMTokens: 0.174978128, // ≈ $2.00 / Mtoken
		Notes:               "Fireworks deepseek-v4-pro — v1 planner + compiler-escalation frontier",
	},
	{
		Model:               ModelKimiK26,
		Group:               GroupReason,
		InputPaxPerMTokens:  0.069991251, // ≈ $0.80 / Mtoken
		OutputPaxPerMTokens: 0.139982502, // ≈ $1.60 / Mtoken
		Notes:               "Fireworks kimi-k2.6 — v1 executor upgrade (general agentic + prose)",
	},
	{
		Model:               ModelGLM5p1Fast,
		Group:               GroupSummarize,
		InputPaxPerMTokens:  0.026246719, // ≈ $0.30 / Mtoken  [PLACEHOLDER — Andrew to set real glm-5.1 rate]
		OutputPaxPerMTokens: 0.078740157, // ≈ $0.90 / Mtoken  [PLACEHOLDER — Andrew to set real glm-5.1 rate]
		Notes:               "Fireworks glm-5p1-fast — Neo's cheap background model (write-back/compaction/validation). PLACEHOLDER rate (deepseek-v4-flash tier) pending real provider price.",
	},
}

// rateIndex maps model id -> Rate. Initialised once at package load.
var rateIndex = func() map[string]Rate {
	m := make(map[string]Rate, len(rateTable))
	for _, r := range rateTable {
		m[r.Model] = r
	}
	return m
}()

// All returns a snapshot of the rate table. The returned slice is
// safe to mutate; callers receive a copy.
func All() []Rate {
	out := make([]Rate, len(rateTable))
	copy(out, rateTable)
	return out
}

// Lookup returns the Rate for a model id, or (Rate{}, false) when the
// model is not on the rate card. Callers MUST treat false as "do not
// debit" — pricing an unknown model would either over-bill or
// under-bill, both of which are incorrect.
func Lookup(model string) (Rate, bool) {
	r, ok := rateIndex[model]
	return r, ok
}

// FreeTierWhitelist returns the slot -> model whitelist enforced by
// internal/routing. Exported for tests + introspection.
func FreeTierWhitelist() map[string][]string {
	return map[string][]string{
		// compiler: gpt-oss-120b is the primary; deepseek-v4-pro is the
		// low-confidence escalation target (compile.go re-invokes the
		// compiler slot with the stronger model when the frame call
		// self-reports confidence below threshold or emits an invalid verb).
		"compiler": {ModelCompilerFreeTier, ModelDeepSeekV4Pro},
		// executor: deepseek-v4-flash stays allowed (summarize / long-ctx +
		// back-compat); kimi-k2.6 is the v1 launch executor default pinned
		// by the router via MATRIX_EXECUTOR_MODEL.
		"executor": {ModelExecutorFreeTier, ModelKimiK26},
		// planner: the daemon now has a dedicated -planner-model knob
		// (MATRIX_PLANNER_MODEL), decoupled from the executor knob. The
		// launch pins planner = kimi-k2.6 (strong tool/JSON fidelity + low
		// hallucination for plan_tree@1 synthesis); deepseek-v4-pro,
		// gpt-oss-120b + v4-flash stay allowed for deployments that pin a
		// different planner. All are on the rate card. Adding kimi-k2.6 here
		// is a whitelist-only change (it is already priced as the executor
		// model) — no rateTable row, no RateTableVersion bump.
		"planner": {ModelKimiK26, ModelCompilerFreeTier, ModelExecutorFreeTier, ModelDeepSeekV4Pro},
		// liaison: the user-facing conversational narrator (SlotLiaison).
		// Reuses already-priced models so adding it needs NO rateTable row
		// and NO RateTableVersion bump. deepseek-v4-flash is the fast/cheap
		// default (1M context for folding large event batches); kimi-k2.6 is
		// the warmer-prose upgrade, pinned via MATRIX_LIAISON_MODEL.
		"liaison": {ModelDeepSeekV4Flash, ModelKimiK26, ModelDeepSeekV4Pro},
		// neo: the Neo default conversational AGENT (SlotNeo). NOT the
		// Liaison — Neo drives the conversation + tools and delegates money
		// to MCL. main = kimi-k2.6 (already priced; shared with executor/
		// planner/liaison); cheap = glm-5p1-fast (background write-back/
		// compaction/validation), added to the rate card in v4.
		"neo": {ModelKimiK26, ModelGLM5p1Fast},
	}
}

// Cost computes the PAX cost for a (model, prompt, completion) tuple.
// Returns a base-PAX decimal string ("0.000123") suitable for the
// X-Matrix-Cost-Pax response header AND the credit_ledger.cost_pax
// NUMERIC(20,12) column. Errors when the model is not on the rate card.
//
// The math uses big.Float internally so the divide-by-1e6 step
// preserves precision at NUMERIC(20,12) granularity. Output is
// truncated (NOT rounded) at 12 decimals to match the schema; we never
// round UP because that would over-bill.
func Cost(model string, promptTokens, completionTokens int) (string, error) {
	if promptTokens < 0 || completionTokens < 0 {
		return "", fmt.Errorf("rates.Cost: negative tokens (prompt=%d completion=%d)",
			promptTokens, completionTokens)
	}
	r, ok := Lookup(model)
	if !ok {
		return "", fmt.Errorf("rates.Cost: model %q not in rate table v%d", model, RateTableVersion)
	}
	// big.Float math: cost = (in*rin + out*rout) / 1e6
	// 256-bit precision is overkill for NUMERIC(20,12) but keeps the
	// rounding error well below the column's least-significant digit.
	prec := uint(256)
	in := new(big.Float).SetPrec(prec).SetInt64(int64(promptTokens))
	out := new(big.Float).SetPrec(prec).SetInt64(int64(completionTokens))
	rin := new(big.Float).SetPrec(prec).SetFloat64(r.InputPaxPerMTokens)
	rout := new(big.Float).SetPrec(prec).SetFloat64(r.OutputPaxPerMTokens)
	million := new(big.Float).SetPrec(prec).SetInt64(1_000_000)

	num := new(big.Float).SetPrec(prec).Add(
		new(big.Float).SetPrec(prec).Mul(in, rin),
		new(big.Float).SetPrec(prec).Mul(out, rout),
	)
	cost := new(big.Float).SetPrec(prec).Quo(num, million)

	return formatPax(cost), nil
}

// AddPax sums two PAX-denominated decimal strings. Returns "" + an
// error when either input fails to parse. Used by the ledger to
// roll up the daily-spend trailer header without re-querying Postgres.
func AddPax(a, b string) (string, error) {
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	prec := uint(256)
	af, _, err := big.ParseFloat(strings.TrimSpace(a), 10, prec, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("rates.AddPax: parse %q: %w", a, err)
	}
	bf, _, err := big.ParseFloat(strings.TrimSpace(b), 10, prec, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("rates.AddPax: parse %q: %w", b, err)
	}
	sum := new(big.Float).SetPrec(prec).Add(af, bf)
	return formatPax(sum), nil
}

// SubPax returns max(a-b, 0) in PAX-denominated decimal-string form.
// Used to compute X-Matrix-Daily-Remaining-Pax without re-querying.
func SubPax(a, b string) (string, error) {
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	prec := uint(256)
	af, _, err := big.ParseFloat(strings.TrimSpace(a), 10, prec, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("rates.SubPax: parse %q: %w", a, err)
	}
	bf, _, err := big.ParseFloat(strings.TrimSpace(b), 10, prec, big.ToNearestEven)
	if err != nil {
		return "", fmt.Errorf("rates.SubPax: parse %q: %w", b, err)
	}
	diff := new(big.Float).SetPrec(prec).Sub(af, bf)
	if diff.Sign() < 0 {
		return "0", nil
	}
	return formatPax(diff), nil
}

// CmpPax compares two PAX-denominated decimal strings. Returns -1, 0,
// or 1 (a < b, a == b, a > b). Errors propagate parse failures.
func CmpPax(a, b string) (int, error) {
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	prec := uint(256)
	af, _, err := big.ParseFloat(strings.TrimSpace(a), 10, prec, big.ToNearestEven)
	if err != nil {
		return 0, fmt.Errorf("rates.CmpPax: parse %q: %w", a, err)
	}
	bf, _, err := big.ParseFloat(strings.TrimSpace(b), 10, prec, big.ToNearestEven)
	if err != nil {
		return 0, fmt.Errorf("rates.CmpPax: parse %q: %w", b, err)
	}
	return af.Cmp(bf), nil
}

// DailyRemainingPax returns max(DailyFreeTierLimitPax - spentPax, 0) as
// a PAX decimal string — the value internal/routing stamps into the
// X-Matrix-Daily-Remaining-Pax header. Thin wrapper over SubPax so the
// cap lives in exactly one place (DailyFreeTierLimitPax).
func DailyRemainingPax(spentPax string) (string, error) {
	return SubPax(DailyFreeTierLimitPax, spentPax)
}

// ExceedsDailyLimit reports whether spentPax has reached or passed the
// free-tier cap (spentPax >= DailyFreeTierLimitPax). True means the
// caller must flip X-Matrix-BYO-API-Key to continue. Errors propagate
// parse failures from CmpPax.
func ExceedsDailyLimit(spentPax string) (bool, error) {
	cmp, err := CmpPax(spentPax, DailyFreeTierLimitPax)
	if err != nil {
		return false, err
	}
	return cmp >= 0, nil
}

// formatPax stringifies a big.Float as a fixed-point decimal at 12
// decimals (matching NUMERIC(20,12)). Trailing zeros are NOT trimmed
// so the wire format is stable across rows.
func formatPax(f *big.Float) string {
	return f.Text('f', 12)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
