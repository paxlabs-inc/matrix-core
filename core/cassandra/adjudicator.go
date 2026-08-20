// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import (
	"context"
	"errors"
	"fmt"
)

// adjudicator.go — the tiered adjudicator ([adjudicator]). It is model- and
// rail-agnostic: the caller injects one or two Decoders (an LLM call adapter)
// and the slot/metering is the caller's concern. A single Primary decoder with
// no Escalate reproduces the legacy single-model critic exactly (the MCL
// re-home posture); supplying Escalate enables the "stronger model on low
// certainty / high stakes" tier for Neo and future consumers.

// Decoder runs one auditor LLM call: system + user prompt in, raw model text
// out. Implementations adapt whatever client the rail already uses (core/mcl/llm,
// Neo's llm package) so cassandra imports none of them.
type Decoder interface {
	Decode(ctx context.Context, system, user string) (string, error)
}

// DefaultLowCertainty is the certainty floor below which a high-stakes audit
// re-runs on the Escalate decoder (when one is configured).
const DefaultLowCertainty = 0.5

// Adjudicator renders a Verdict from an AuditInput.
type Adjudicator struct {
	// Primary is the cheap/fast auditor. Required.
	Primary Decoder
	// Escalate is the stronger auditor consulted on low-certainty, high-stakes
	// audits. Optional; nil disables escalation (single-call behaviour).
	Escalate Decoder
	// LowCertainty overrides DefaultLowCertainty when > 0.
	LowCertainty float64
}

// AuditInput is everything the adjudicator needs to render one verdict.
type AuditInput struct {
	// Request is the user's original request / contract to satisfy.
	Request string
	// Evidence is the ground-truth execution digest (real tool calls + real
	// results), built by the caller from its own rail (MCL plan tree / Neo
	// working transcript).
	Evidence string
	// HighStakes marks a turn that touched an irreversible seam (money / chain
	// / file-overwrite / external-write) or makes claims-of-fact about
	// external state. Such turns may escalate to the stronger auditor
	// ([coupling].high_stakes).
	HighStakes bool
	// Priors are the deterministic pre-pass signals; attached to the prompt as
	// an advisory and used to gate escalation. Zero value = no priors.
	Priors PriorSignal
}

// Adjudicate renders the Verdict. On error (decode/parse failure) the caller is
// expected to FAIL OPEN on reversible turns ([principles].fail_open_reversible,
// invariant i_cass_5) — cassandra surfaces the error and never decides for them.
func (a *Adjudicator) Adjudicate(ctx context.Context, in AuditInput) (*Verdict, error) {
	if a == nil || a.Primary == nil {
		return nil, errors.New("cassandra: adjudicator has no primary decoder")
	}
	v, err := a.decode(ctx, a.Primary, in)
	if err != nil {
		return nil, err
	}
	// Tiered escalation: a stronger auditor renders a second opinion only when
	// configured, the turn is high-stakes, and the primary verdict is
	// low-certainty. The STRICTER of the two verdicts is kept, so escalation can
	// only ever HOLD or TIGHTEN the primary — it can never flip a refusing
	// primary into an acceptance (C3 / req 7.2). Escalation failure is
	// non-fatal: the primary verdict stands.
	if a.Escalate != nil && in.HighStakes && v.Certainty < a.lowCertainty() {
		if ev, eerr := a.decode(ctx, a.Escalate, in); eerr == nil {
			return stricter(v, ev), nil
		}
	}
	return v, nil
}

// stricter returns the more-conservative of two verdicts, biasing toward
// refusal ([principles].coherence_toward_more_work, req 7.2). A verdict that
// does NOT accept (Sound is false) is stricter than one that does, so when the
// primary and the escalated auditor disagree on the accept/refuse decision the
// refusing verdict wins — escalation tightens, never loosens. When both agree
// on that decision, the more-certain reading is kept.
func stricter(primary, escalated *Verdict) *Verdict {
	if primary == nil {
		return escalated
	}
	if escalated == nil {
		return primary
	}
	ps, es := primary.Sound(), escalated.Sound()
	if ps != es {
		if !ps {
			return primary
		}
		return escalated
	}
	if escalated.Certainty > primary.Certainty {
		return escalated
	}
	return primary
}

func (a *Adjudicator) lowCertainty() float64 {
	if a.LowCertainty > 0 {
		return a.LowCertainty
	}
	return DefaultLowCertainty
}

// decode runs one decoder, parses, and normalizes the verdict.
func (a *Adjudicator) decode(ctx context.Context, d Decoder, in AuditInput) (*Verdict, error) {
	raw, err := d.Decode(ctx, SystemPrompt, BuildAuditPrompt(in))
	if err != nil {
		return nil, fmt.Errorf("cassandra: decode: %w", err)
	}
	v, perr := ParseVerdict(raw)
	if perr != nil {
		return nil, fmt.Errorf("cassandra: parse: %w", perr)
	}
	return v, nil
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
