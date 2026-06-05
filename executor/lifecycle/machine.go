// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"matrix/mcl/envelope"
)

// Lifecycle errors.
var (
	ErrInvalidTransition = errors.New("lifecycle: invalid transition")
	ErrTerminal          = errors.New("lifecycle: state is terminal")
	ErrUnknownState      = errors.New("lifecycle: unknown state")
	ErrUnknownKind       = errors.New("lifecycle: message kind does not drive a transition")
	ErrEnvelopeMismatch  = errors.New("lifecycle: envelope intent mismatch")
	ErrAlreadyAtTerminal = errors.New("lifecycle: intent already at terminal state")
	ErrCorrectionPolicy  = errors.New("lifecycle: correction policy violated")
)

// Event records a single lifecycle transition. Persisted into both the
// envelope log (journal/logs/...) and cortex (as an Event memory) so
// the lifecycle is queryable via cortex.find AND auditable via the
// envelope chain.
type Event struct {
	// IntentID is the intent.id this transition applies to.
	IntentID string

	// From + To are the lifecycle states. From==To is permitted for the
	// non-material correction no-op.
	From State
	To   State

	// Kind is the MCL message kind that caused the transition.
	Kind string

	// EnvelopeID is the id of the triggering envelope (audit).
	EnvelopeID string

	// At is the wall-clock at apply time.
	At time.Time

	// Material flags whether this transition was triggered by a
	// material correction (D9 classification). Only meaningful when
	// Kind == intent.correct.
	Material bool

	// Notes is free-text for human audit (e.g. failure reason).
	Notes string
}

// Machine is a per-intent lifecycle state holder. Thread-safe. Apply
// a transition by calling Apply with an envelope; reject and remain at
// the current state if the move is invalid.
//
// The Machine itself does NOT persist state; the caller is responsible
// for journaling Event records and reconstructing on restart.
type Machine struct {
	mu       sync.Mutex
	intentID string
	state    State
	history  []Event
	// MaxHistory caps the in-memory history slice for long-running
	// intents. Zero disables the cap (unbounded; for tests / short-run).
	MaxHistory int
}

// New constructs a Machine for the given intent at the initial state
// (typically StateDrafting). Returns an error if state is not a valid
// lifecycle state.
func New(intentID string, initial State) (*Machine, error) {
	if intentID == "" {
		return nil, errors.New("lifecycle: empty intent_id")
	}
	if !ValidState(initial) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownState, initial)
	}
	return &Machine{
		intentID: intentID,
		state:    initial,
	}, nil
}

// State returns the current lifecycle state.
func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// IntentID returns the bound intent id.
func (m *Machine) IntentID() string {
	return m.intentID
}

// History returns a copy of the lifecycle events in arrival order.
func (m *Machine) History() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.history))
	copy(out, m.history)
	return out
}

// ApplyOpts configures Apply behaviour for kinds that need extra context
// (specifically intent.correct, which needs the materiality flag).
type ApplyOpts struct {
	// Now overrides the wall-clock used for the Event.At field. Zero
	// defers to time.Now(). Tests always populate this for determinism.
	Now time.Time

	// Material is the D9 materiality classification result for the
	// intent.correct kind. Ignored for other kinds.
	Material bool

	// Notes is captured into Event.Notes verbatim.
	Notes string
}

// Apply attempts to transition based on the given envelope. Returns the
// new state and the recorded Event. On invalid transitions, returns an
// error and leaves the state unchanged.
//
// The kind→transition table (MessageKindTransition) drives which target
// state each envelope kind moves to from the current state. Kinds that
// do not drive transitions (plan.step, plan.output, policy.gate,
// policy.gate.resolve) return ErrUnknownKind so the caller can opt to
// ignore them at the lifecycle layer.
func (m *Machine) Apply(env *envelope.Envelope, opts ApplyOpts) (State, *Event, error) {
	if env == nil {
		return "", nil, errors.New("lifecycle: nil envelope")
	}
	if env.Intent != "matrix://intent/"+m.intentID && env.Intent != m.intentID {
		return "", nil, fmt.Errorf("%w: envelope intent=%s machine=%s",
			ErrEnvelopeMismatch, env.Intent, m.intentID)
	}

	target, ok := lookupTransition(env.Kind, opts.Material)
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", ErrUnknownKind, env.Kind)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.IsTerminal() {
		return m.state, nil, fmt.Errorf("%w: state=%s kind=%s", ErrAlreadyAtTerminal, m.state, env.Kind)
	}

	if !Transition(m.state, target) {
		return m.state, nil, fmt.Errorf("%w: %s on kind=%s", ErrInvalidTransition,
			formatTransition(m.state, target), env.Kind)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	evt := Event{
		IntentID:   m.intentID,
		From:       m.state,
		To:         target,
		Kind:       env.Kind,
		EnvelopeID: env.ID,
		At:         now,
		Material:   opts.Material && env.Kind == envelope.KindIntentCorrect,
		Notes:      opts.Notes,
	}

	m.state = target
	m.history = append(m.history, evt)
	if m.MaxHistory > 0 && len(m.history) > m.MaxHistory {
		// Drop oldest entries; keep tail. Audit log is the authoritative
		// history — in-memory ring is for convenience only.
		m.history = m.history[len(m.history)-m.MaxHistory:]
	}

	return target, &evt, nil
}

// MessageKindTransition is the documentation-table of which message
// kinds drive which lifecycle transitions. Exported for the executor's
// audit + UI surfaces.
//
// Entries map: kind → (TransitionEffect describing the move).
type TransitionEffect struct {
	// To is the target state. For intent.correct this is selected at
	// Apply time based on the Material flag (material → accepted,
	// non-material → executing).
	To State

	// MaterialTo is the alternate target for intent.correct when the
	// Material flag is true. Unused for other kinds.
	MaterialTo State

	// Description is human-readable.
	Description string
}

// MessageKindTransition is the table consulted by lookupTransition.
// Kinds not in this map do not drive lifecycle transitions.
var MessageKindTransition = map[string]TransitionEffect{
	envelope.KindIntentCompiled: {To: StateProposed, Description: "agent emits typed IR"},
	envelope.KindIntentClarify:  {To: StateClarifying, Description: "agent asks structured questions"},
	envelope.KindIntentAnswer:   {To: StateProposed, Description: "user answers; re-compile loop"},
	envelope.KindIntentAccept:   {To: StateAccepted, Description: "user signs the IR"},
	envelope.KindPlanProposed:   {To: StateExecuting, Description: "executor begins plan walk"},
	envelope.KindIntentAttest:   {To: StateCompleted, Description: "successful completion"},
	envelope.KindIntentFail:     {To: StateFailed, Description: "typed failure"},
	envelope.KindIntentCancel:   {To: StateCancelled, Description: "user revokes"},
	envelope.KindIntentCorrect: {
		To:          StateExecuting, // non-material default
		MaterialTo:  StateAccepted,  // material: rewind to accept loop
		Description: "user patches mid-flight; target depends on materiality (D9)",
	},
}

// lookupTransition returns the target state for the given envelope
// kind, taking into account the materiality flag for intent.correct.
func lookupTransition(kind string, material bool) (State, bool) {
	eff, ok := MessageKindTransition[kind]
	if !ok {
		return "", false
	}
	if kind == envelope.KindIntentCorrect && material {
		return eff.MaterialTo, true
	}
	return eff.To, true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
