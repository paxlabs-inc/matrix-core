// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package controller is the resurrection runtime's silent voice: the one
// Cassandra controller, wielding the in-place doubt edit as an instrument.
//
// It carries the matrix Cassandra 2.0 cadence verbatim (min_step 2, at most
// 3 mods per turn, a 2-step per-trigger cooldown) and ion's edit discipline
// (dual-record provenance, an auditor side-channel, a bounded undo window).
// The edit itself never touches durable state: the loop folds the returned
// line into a request-time clone for exactly one provider call, so controller
// guidance never reaches the durable transcript or the Neocortex transcript.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/cortexclient"
)

const (
	// DefaultMinStep, DefaultMaxModsPerTurn, and DefaultCooldownSteps are the
	// matrix Cassandra 2.0 cadence bounds, preserved verbatim from
	// neo/internal/agent casMinStep / casMaxMods / casCooldownSteps.
	DefaultMinStep        = 2
	DefaultMaxModsPerTurn = 3
	DefaultCooldownSteps  = 2
	// UndoWindow is ion's bounded undo window for a recorded edit.
	UndoWindow = 72 * time.Hour
)

type Trigger string

const (
	// TriggerPredictionMismatch fires when a committed ToolEvent's prediction
	// did not match its result.
	TriggerPredictionMismatch Trigger = "prediction_mismatch"
	// TriggerRefutedPremise fires when the plan still rests on a premise a
	// verified citation has refuted.
	TriggerRefutedPremise Trigger = "refuted_premise"
)

type Side string

const (
	SideDoubt     Side = "doubt"
	SideAssurance Side = "assurance"
)

type EditState string

const (
	EditActive EditState = "active"
	EditUndone EditState = "undone"
)

// Cadence bounds the controller. A zero field resolves to its matrix default.
type Cadence struct {
	MinStep        int
	MaxModsPerTurn int
	CooldownSteps  int
}

func (cadence Cadence) resolved() Cadence {
	if cadence.MinStep <= 0 {
		cadence.MinStep = DefaultMinStep
	}
	if cadence.MaxModsPerTurn <= 0 {
		cadence.MaxModsPerTurn = DefaultMaxModsPerTurn
	}
	if cadence.CooldownSteps <= 0 {
		cadence.CooldownSteps = DefaultCooldownSteps
	}
	return cadence
}

// Signals is the per-step read the controller classifies. It holds only what
// the loop already computed.
type Signals struct {
	Step               int
	MismatchedEvidence int
	RefutedPremises    int
	Citation           *cortexclient.ToolEventCitation
}

// Mod is the dual record for one silent modification: what the model actually
// said, what the controller folded in, and the evidence that armed it.
type Mod struct {
	ID       string                          `json:"id"`
	Step     int                             `json:"step"`
	Line     string                          `json:"line"`
	Trigger  Trigger                         `json:"trigger"`
	Side     Side                            `json:"side"`
	Citation *cortexclient.ToolEventCitation `json:"citation,omitempty"`
	At       time.Time                       `json:"at"`
	State    EditState                       `json:"state"`
	UndoneAt *time.Time                      `json:"undone_at,omitempty"`
}

// Auditor durably records every controller action. Optional: a nil auditor
// keeps the controller a pure in-memory instrument.
type Auditor interface {
	RecordDoubtEdit(Mod) error
}

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type Controller struct {
	mu           sync.Mutex
	cadence      Cadence
	clock        Clock
	auditor      Auditor
	mods         []Mod
	modsThisTurn int
	cooldown     map[Trigger]int
}

func New(cadence Cadence, auditor Auditor, clock Clock) *Controller {
	if clock == nil {
		clock = systemClock{}
	}
	return &Controller{
		cadence:  cadence.resolved(),
		clock:    clock,
		auditor:  auditor,
		cooldown: make(map[Trigger]int),
	}
}

// Cadence returns the resolved bounds this controller enforces.
func (controller *Controller) Cadence() Cadence { return controller.cadence }

// Observe classifies the step and, when a doubt edit is warranted and the
// cadence allows it, records the dual record and returns the line for the
// loop to fold into a request-time clone. Silence when healthy: no trigger,
// no mod, and the request is byte-identical to the controller absent.
func (controller *Controller) Observe(signals Signals) (Mod, bool) {
	trigger, side := classify(signals)
	if trigger == "" {
		return Mod{}, false
	}
	line := template(trigger)
	if line == "" {
		return Mod{}, false
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if signals.Step < controller.cadence.MinStep ||
		controller.modsThisTurn >= controller.cadence.MaxModsPerTurn {
		return Mod{}, false
	}
	if last, ok := controller.cooldown[trigger]; ok &&
		signals.Step-last < controller.cadence.CooldownSteps {
		return Mod{}, false
	}
	now := controller.clock.Now().UTC()
	mod := Mod{
		ID:       modID(trigger, signals.Step, line, now),
		Step:     signals.Step,
		Line:     line,
		Trigger:  trigger,
		Side:     side,
		Citation: signals.Citation,
		At:       now,
		State:    EditActive,
	}
	if controller.auditor != nil {
		if err := controller.auditor.RecordDoubtEdit(mod); err != nil {
			return Mod{}, false
		}
	}
	controller.mods = append(controller.mods, mod)
	controller.modsThisTurn++
	controller.cooldown[trigger] = signals.Step
	return mod, true
}

// Undo marks a recorded edit undone inside the bounded window. The edit only
// ever rode a request-time clone, so undo is a provenance operation: the mod
// is marked and audited, and it can never be re-applied.
func (controller *Controller) Undo(id string) (Mod, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.clock.Now().UTC()
	for index := len(controller.mods) - 1; index >= 0; index-- {
		mod := &controller.mods[index]
		if mod.ID != id || mod.State != EditActive {
			continue
		}
		if now.Sub(mod.At) > UndoWindow {
			return Mod{}, fmt.Errorf(
				"runtime controller: undo window expired for %s", id,
			)
		}
		undone := *mod
		undone.State = EditUndone
		undone.UndoneAt = &now
		if controller.auditor != nil {
			if err := controller.auditor.RecordDoubtEdit(undone); err != nil {
				return Mod{}, fmt.Errorf(
					"runtime controller: audit undo: %w", err,
				)
			}
		}
		*mod = undone
		return undone, nil
	}
	return Mod{}, fmt.Errorf("runtime controller: no active edit %s", id)
}

// Mods returns a defensive copy of the dual records.
func (controller *Controller) Mods() []Mod {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	result := make([]Mod, len(controller.mods))
	copy(result, controller.mods)
	return result
}

// NewTurn resets the per-turn mod budget and the cooldown ledger.
func (controller *Controller) NewTurn() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.modsThisTurn = 0
	controller.cooldown = make(map[Trigger]int)
}

// classify maps the step's evidence to (trigger, side). A refuted premise
// outranks a bare mismatch: the plan is known wrong at a load-bearing point.
func classify(signals Signals) (Trigger, Side) {
	if signals.RefutedPremises > 0 {
		return TriggerRefutedPremise, SideDoubt
	}
	if signals.MismatchedEvidence > 0 {
		return TriggerPredictionMismatch, SideDoubt
	}
	return "", ""
}

// template composes the first-person line. Metacognition only: epistemic
// framing about the agent's own work, never a claim of fact and never a
// fabricated completion. The refuted-premise line is the matrix Cassandra 2.0
// text, preserved verbatim.
func template(trigger Trigger) string {
	switch trigger {
	case TriggerRefutedPremise:
		return "Hold on — a premise my plan rests on has been REFUTED (it's " +
			"marked in my premise ledger). Anything I do that depends on it " +
			"is built on a known falsehood. I need to revise the plan around " +
			"the refuted premise before taking another dependent step."
	case TriggerPredictionMismatch:
		return "Hold on — what that step actually returned is not what I " +
			"predicted it would return. The prediction is recorded against " +
			"the evidence, so the mismatch is real, not a misreading. Before " +
			"I take another step that depends on it, I need to revise the " +
			"plan around what the evidence actually says."
	}
	return ""
}

func modID(trigger Trigger, step int, line string, at time.Time) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%d\x00%s\x00%d", trigger, step, line, at.UnixNano())
	return hex.EncodeToString(digest.Sum(nil))[:32]
}

// FoldInto returns a copy of content with the controller line layered after
// it as the agent's next thought. The original is always preserved, so the
// fold is additive and can never rewrite a fact the model stated.
func FoldInto(content string, line string) string {
	if strings.TrimSpace(content) == "" {
		return line
	}
	return content + "\n\n" + line
}
