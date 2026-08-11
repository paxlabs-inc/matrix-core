// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package schema

import (
	"encoding/json"
	"fmt"

	"matrix/construct/schema/primitives"
)

// Surface is the on-the-wire envelope every projected surface rides in. It is
// a discriminated union over Kind: exactly one of the 8 payload pointers is
// set, matching Kind. The common envelope fields (id, ref, seq, parent,
// attributes) decorate any kind without duplication (invariant i4).
//
// The optional-payload-pointer shape (rather than a flattened union) keeps
// stdlib JSON round-trip trivial and the TS codegen a 1:1 struct->interface
// mapping; the renderer dispatches on Kind and reads the matching payload.
type Surface struct {
	// Kind is the primitive discriminator (one of the frozen 8).
	Kind Kind `json:"kind"`
	// ID is the stable surface identity, used to target progressive patches
	// and to be the target of a ref from another surface.
	ID string `json:"id"`
	// Ref optionally links this surface to another by its ID (frozen
	// [attributes].ref — the linking axis lives on the envelope).
	Ref string `json:"ref,omitempty"`
	// Seq orders surfaces within a run; mirrors the transcript event seq.
	Seq uint64 `json:"seq,omitempty"`
	// Parent optionally nests this surface under another (composition).
	Parent string `json:"parent,omitempty"`
	// Attributes decorate the surface (stakes/confidence/cost/temporality).
	Attributes *Attributes `json:"attributes,omitempty"`

	// Exactly one of the following is set, selected by Kind.
	Narration *primitives.Narration `json:"narration,omitempty"`
	Metric    *primitives.Metric    `json:"metric,omitempty"`
	Entity    *primitives.Entity    `json:"entity,omitempty"`
	Structure *primitives.Structure `json:"structure,omitempty"`
	Stream    *primitives.Stream    `json:"stream,omitempty"`
	Timeline  *primitives.Timeline  `json:"timeline,omitempty"`
	Canvas    *primitives.Canvas    `json:"canvas,omitempty"`
	Ask       *primitives.Ask       `json:"ask,omitempty"`
}

// payloadCount reports how many payload pointers are non-nil. A valid surface
// has exactly one.
func (s *Surface) payloadCount() int {
	n := 0
	if s.Narration != nil {
		n++
	}
	if s.Metric != nil {
		n++
	}
	if s.Entity != nil {
		n++
	}
	if s.Structure != nil {
		n++
	}
	if s.Stream != nil {
		n++
	}
	if s.Timeline != nil {
		n++
	}
	if s.Canvas != nil {
		n++
	}
	if s.Ask != nil {
		n++
	}
	return n
}

// payloadMatchesKind reports whether the single non-nil payload is the one
// selected by Kind.
func (s *Surface) payloadMatchesKind() bool {
	switch s.Kind {
	case KindNarration:
		return s.Narration != nil
	case KindMetric:
		return s.Metric != nil
	case KindEntity:
		return s.Entity != nil
	case KindStructure:
		return s.Structure != nil
	case KindStream:
		return s.Stream != nil
	case KindTimeline:
		return s.Timeline != nil
	case KindCanvas:
		return s.Canvas != nil
	case KindAsk:
		return s.Ask != nil
	default:
		return false
	}
}

// Validate checks the structural contract: a known kind, a non-empty id,
// exactly one payload matching the kind, and in-range attributes. It does NOT
// validate the deep semantics of each payload beyond the enum/required checks
// the payload kinds expose, keeping the envelope cheap to gate at the
// transport boundary.
func (s *Surface) Validate() error {
	if !ValidKind(s.Kind) {
		return fmt.Errorf("construct: unknown surface kind %q", s.Kind)
	}
	if s.ID == "" {
		return fmt.Errorf("construct: surface id is required")
	}
	if n := s.payloadCount(); n != 1 {
		return fmt.Errorf("construct: surface must carry exactly one payload, found %d", n)
	}
	if !s.payloadMatchesKind() {
		return fmt.Errorf("construct: surface payload does not match kind %q", s.Kind)
	}
	if !s.Attributes.Valid() {
		return fmt.Errorf("construct: surface attributes out of range")
	}
	if err := s.validatePayload(); err != nil {
		return err
	}
	return nil
}

// validatePayload runs the per-primitive enum/required checks for the active
// payload.
func (s *Surface) validatePayload() error {
	switch s.Kind {
	case KindStructure:
		if !primitives.ValidShape(s.Structure.Shape) {
			return fmt.Errorf("construct: unknown structure shape %q", s.Structure.Shape)
		}
	case KindTimeline:
		for _, st := range s.Timeline.Steps {
			if !primitives.ValidStepStatus(st.Status) {
				return fmt.Errorf("construct: unknown timeline step status %q", st.Status)
			}
		}
	case KindAsk:
		if !primitives.ValidAskKind(s.Ask.AskKind) {
			return fmt.Errorf("construct: unknown ask kind %q", s.Ask.AskKind)
		}
	case KindMetric:
		if !primitives.ValidMetricDisplay(s.Metric.Display) {
			return fmt.Errorf("construct: unknown metric display %q", s.Metric.Display)
		}
	case KindCanvas:
		if s.Canvas.Chart != nil && !primitives.ValidChartKind(s.Canvas.Chart.Kind) {
			return fmt.Errorf("construct: unknown chart kind %q", s.Canvas.Chart.Kind)
		}
	}
	return nil
}

// Marshal serialises the surface to canonical JSON bytes.
func (s *Surface) Marshal() ([]byte, error) {
	return json.Marshal(s)
}

// Unmarshal parses a surface from JSON bytes. It does not validate; call
// Validate separately at trust boundaries.
func Unmarshal(b []byte) (*Surface, error) {
	var s Surface
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ParseValid parses and validates a surface in one step (the transport-layer
// entry point).
func ParseValid(b []byte) (*Surface, error) {
	s, err := Unmarshal(b)
	if err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}
