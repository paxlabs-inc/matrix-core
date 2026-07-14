// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// signals.go — the unified per-step signal state (MORPHEUS req.5.1): ONE
// behavioral read of the assistant's current batch, computed ONCE per step
// (deliberate) and shared by every self-correction consumer — the stall
// commit (noteBatch), the Cassandra controller (cassandraStep via
// cassandraView), and the governor assembled over it in wave 8. Before the
// reassembly, noteBatch and buildCassandraSignals each recomputed the exact/
// semantic/cyclic reads privately; readStepSignals is now the only place they
// are computed.
package agent

import "matrix/neo/internal/llm"

// stepSignals is the one per-step signal state. It is a READ of the turn's
// live state at step entry — the stall bookkeeping still reflects the PREVIOUS
// committed batch (noteBatch commits later in act), so the repeat fields are
// the same one-step-early verdict the Cassandra controller has always read,
// and effectiveRepeats is exactly the value the commit will produce.
type stepSignals struct {
	step    int
	calls   []llm.ToolCall // this step's tool batch (empty on a bare-answer close)
	sig     string         // batch signature (byte identity)
	closing bool           // this turn ends on a bare answer (no tool calls)

	// Repeat reads against the previous committed batch — computed HERE only.
	exact             bool // byte-identical batch signature
	semantic          bool // same operation, reworded arguments
	cyclic            bool // rotating A→B→A→B, no new tool
	repeat            bool // any of the three
	effectiveRepeats  int  // committed repeats including this batch's read
	introducedNewTool bool // the batch names a tool not yet dispatched this turn

	// Breadth: distinct tools dispatched BEFORE this batch, and whether any
	// tool ran earlier this turn.
	toolBreadth int
	workDone    bool

	// Premise-ledger reads (epistemic-core req.4.3): standing refuted premises
	// awaiting revision, and unchecked self-referential assumptions.
	refutedPremises        int
	ungroundedSelfPremises int

	// Epistemic meters (read views of the turn's live source state): the
	// per-strategy consecutive prediction-mismatch counts, and the task
	// graph's evidence-set size + consecutive actions that grew no evidence.
	mismatch           map[string]int
	evidenceItems      int
	actionsSinceGrowth int

	// The unified unproductive-attempt counter at step entry.
	unproductive int
}

// readStepSignals is THE one computation of the per-step behavioral read
// (req.5.1). The stall inputs (repeats/recentSigs/prevSig/prevCalls/distinct)
// are explicit so the same core serves both the live turn state
// (computeStepSignals) and an explicitly-supplied state (buildCassandraSignals,
// kept for the controller's test surface). The ledger/graph/counter reads come
// from the live turn.
func (a *Agent) readStepSignals(step int, calls []llm.ToolCall, repeats int, recentSigs []string, prevSig string, prevCalls []llm.ToolCall, distinct map[string]struct{}, closing bool) *stepSignals {
	sig := batchSignature(calls)
	introducedNewTool := false
	for _, c := range calls {
		if _, seen := distinct[c.Function.Name]; !seen {
			introducedNewTool = true
			break
		}
	}
	cyclic := len(calls) > 0 && !introducedNewTool && sigInWindow(recentSigs, sig)
	semantic := a.semanticRepeat(prevCalls, calls)
	exact := len(calls) > 0 && sig == prevSig
	repeat := exact || semantic || cyclic
	eff := repeats
	if repeat {
		eff = repeats + 1
	}

	t := a.turn
	s := &stepSignals{
		step:                   step,
		calls:                  calls,
		sig:                    sig,
		closing:                closing,
		exact:                  exact,
		semantic:               semantic,
		cyclic:                 cyclic,
		repeat:                 repeat,
		effectiveRepeats:       eff,
		introducedNewTool:      introducedNewTool,
		toolBreadth:            len(distinct),
		workDone:               len(distinct) > 0,
		refutedPremises:        len(t.ledger.unrevisedRefuted()),
		ungroundedSelfPremises: len(t.ledger.ungroundedSelf()),
		mismatch:               t.mismatchMeter,
		unproductive:           t.unproductive,
	}
	if t.graph != nil {
		s.evidenceItems = len(t.graph.evidenceSet)
		s.actionsSinceGrowth = t.graph.actionsSinceGrowth
	}
	return s
}

// computeStepSignals computes the unified signal state for THIS step from the
// turn's live state — called once per step at deliberate entry and stored on
// the turn for every downstream consumer.
func (a *Agent) computeStepSignals(step int, msg llm.Message) *stepSignals {
	t := a.turn
	return a.readStepSignals(step, msg.ToolCalls, t.repeats, t.recentSigs, t.prevSig, t.prevCalls, t.distinctToolSet, len(msg.ToolCalls) == 0)
}

// cassandraView projects the unified state onto the controller's signal shape
// — a pure field mapping, no recomputation.
func (s *stepSignals) cassandraView() cassandraSignals {
	return cassandraSignals{
		step:                   s.step,
		closing:                s.closing,
		calls:                  s.calls,
		effectiveRepeats:       s.effectiveRepeats,
		cyclic:                 s.cyclic,
		semanticRepeat:         s.semantic,
		workDone:               s.workDone,
		refutedPremises:        s.refutedPremises,
		ungroundedSelfPremises: s.ungroundedSelfPremises,
	}
}
