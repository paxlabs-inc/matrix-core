// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package backchannel is the bidirectional half of the Construct: the typed
// human-response round-trip for the one inherently bidirectional primitive,
// Ask (invariant i5). The emit side renders an Ask surface (a choose/input/
// confirm/sign/upload request) and PARKS the agent; the human answers; the
// answer flows back to the agent as an INPUT on the same footing as a user
// message — replayable, deterministic — NEVER a mutation of plan/walk/cortex.
//
// This package owns the response CONTRACT so it stays single-source across the
// emitter (Neo's construct_render handler) and any HTTP receiver: it validates
// a posted response against the Ask it answers, renders a human/agent-readable
// summary of the answer (what the parked agent reads to resume), and folds the
// answer back onto the Ask surface so the renderer can settle its control. It
// imports only schema + primitives, so it is module-decoupled exactly like
// transport and projection.
package backchannel

import (
	"fmt"
	"strings"

	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
)

// ValidateResponse checks a posted AskResponse against the Ask it answers, per
// the Ask kind's expected-response contract. It is the trust gate the receiver
// runs BEFORE delivering the answer to the parked agent: a malformed or
// off-contract response is rejected (the run stays parked) rather than feeding
// the agent garbage. A nil/unknown ask or response is an error.
func ValidateResponse(ask *primitives.Ask, resp *primitives.AskResponse) error {
	if ask == nil {
		return fmt.Errorf("construct/backchannel: nil ask")
	}
	if resp == nil {
		return fmt.Errorf("construct/backchannel: nil response")
	}
	if !primitives.ValidAskKind(ask.AskKind) {
		return fmt.Errorf("construct/backchannel: unknown ask kind %q", ask.AskKind)
	}
	switch ask.AskKind {
	case primitives.AskChoose:
		if strings.TrimSpace(resp.Choice) == "" {
			return fmt.Errorf("construct/backchannel: a choose response requires a choice")
		}
		if len(ask.Options) > 0 && !hasOption(ask.Options, resp.Choice) {
			return fmt.Errorf("construct/backchannel: choice %q is not one of the offered options", resp.Choice)
		}
	case primitives.AskInput:
		if strings.TrimSpace(resp.Value) == "" {
			return fmt.Errorf("construct/backchannel: an input response requires a value")
		}
	case primitives.AskConfirm:
		if resp.Confirmed == nil {
			return fmt.Errorf("construct/backchannel: a confirm response requires confirmed true or false")
		}
	case primitives.AskSign:
		// A real wallet signature satisfies sign; so does an explicit
		// decision (including a denial). The actual signing still crosses the
		// rigorous money rail via core_execute — the Ask only records the
		// human's intent.
		if strings.TrimSpace(resp.Signature) == "" && resp.Confirmed == nil {
			return fmt.Errorf("construct/backchannel: a sign response requires a signature or an explicit decision")
		}
	case primitives.AskUpload:
		if strings.TrimSpace(resp.UploadRef) == "" {
			return fmt.Errorf("construct/backchannel: an upload response requires an upload reference")
		}
	}
	return nil
}

// Summarize renders the human's answer as a short, agent-readable line — the
// tool result the parked agent reads to resume its reasoning. It mirrors the
// client renderer's settled state so the agent and the screen agree on what
// was answered. Caller has already validated the response.
func Summarize(ask *primitives.Ask, resp *primitives.AskResponse) string {
	if ask == nil || resp == nil {
		return "The user did not answer."
	}
	switch ask.AskKind {
	case primitives.AskChoose:
		if label := optionLabel(ask.Options, resp.Choice); label != "" {
			return fmt.Sprintf("The user chose: %s.", label)
		}
		return fmt.Sprintf("The user chose: %s.", resp.Choice)
	case primitives.AskInput:
		return fmt.Sprintf("The user answered: %s", strings.TrimSpace(resp.Value))
	case primitives.AskConfirm:
		if confirmedTrue(resp) {
			return "The user confirmed."
		}
		return "The user declined."
	case primitives.AskSign:
		if sig := strings.TrimSpace(resp.Signature); sig != "" {
			return fmt.Sprintf("The user signed (signature %s).", clip(sig, 24))
		}
		if confirmedTrue(resp) {
			return "The user approved signing; complete the action through the secure path (core_execute)."
		}
		return "The user declined to sign."
	case primitives.AskUpload:
		return fmt.Sprintf("The user uploaded: %s", strings.TrimSpace(resp.UploadRef))
	}
	return "The user answered."
}

// Answered folds a validated response onto a COPY of the original Ask surface,
// returning a surface suitable for a construct.surface.patch emit: the renderer
// reads Ask.Response and flips from the live control to its settled state. The
// original surface is never mutated (the patch is a clean clone).
func Answered(orig *schema.Surface, resp *primitives.AskResponse) (*schema.Surface, error) {
	if orig == nil || orig.Kind != schema.KindAsk || orig.Ask == nil {
		return nil, fmt.Errorf("construct/backchannel: surface is not an ask")
	}
	if resp == nil {
		return nil, fmt.Errorf("construct/backchannel: nil response")
	}
	b, err := orig.Marshal()
	if err != nil {
		return nil, err
	}
	clone, err := schema.Unmarshal(b)
	if err != nil {
		return nil, err
	}
	clone.Ask.Response = resp
	return clone, nil
}

func hasOption(opts []primitives.AskOption, id string) bool {
	for _, o := range opts {
		if o.ID == id {
			return true
		}
	}
	return false
}

func optionLabel(opts []primitives.AskOption, id string) string {
	for _, o := range opts {
		if o.ID == id {
			return o.Label
		}
	}
	return ""
}

func confirmedTrue(resp *primitives.AskResponse) bool {
	return resp.Confirmed != nil && *resp.Confirmed
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
