// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"testing"

	"matrix/neo/internal/config"
)

// captureReporter records every Say call so tests can assert whether output
// was surfaced to the user.
type captureReporter struct {
	said []string
}

func (c *captureReporter) Say(text string)                { c.said = append(c.said, text) }
func (c *captureReporter) Status(string)                  {}
func (c *captureReporter) Notice(string)                  {}
func (c *captureReporter) Think(string)                   {}
func (c *captureReporter) Delta(int, string, string)      {}

// TestHeartbeat_OKSuppressesOutput verifies the core P1-4 invariant: when the
// agent's final answer is the HEARTBEAT_OK sentinel, the turn is SUPPRESSED —
// a.out.Say is NOT called, so HEARTBEAT_OK is never surfaced to the user. The
// model emits this sentinel after reviewing active goals/constraints on a
// Chronos heartbeat wake and finding nothing needing attention.
func TestHeartbeat_OKSuppressesOutput(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	a.finishTurn(context.Background(), HeartbeatOK, nil, nil, HeartbeatWakeMessage)

	if len(rep.said) != 0 {
		t.Fatalf("HEARTBEAT_OK must be suppressed (never surfaced); Say was called %d time(s): %v", len(rep.said), rep.said)
	}
}

// TestHeartbeat_NonOKDelivers verifies that anything other than the HEARTBEAT_OK
// sentinel is delivered normally: a.out.Say IS called with the answer. A
// heartbeat wake that finds something needing attention produces real content,
// which must reach the user.
func TestHeartbeat_NonOKDelivers(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	answer := "The deployment is stuck — the CI pipeline failed at the test stage."
	a.finishTurn(context.Background(), answer, nil, nil, HeartbeatWakeMessage)

	if len(rep.said) != 1 {
		t.Fatalf("non-OK answer must be delivered exactly once; got %d Say calls", len(rep.said))
	}
	if rep.said[0] != answer {
		t.Errorf("delivered text must match the answer; got %q", rep.said[0])
	}
}

// TestHeartbeat_DisabledByDefault verifies that the heartbeat interval defaults
// to 0 (off). No wakeups occur when disabled — the convention is opt-in via
// NEO_HEARTBEAT_INTERVAL or the [heartbeat] kvx section.
func TestHeartbeat_DisabledByDefault(t *testing.T) {
	c := config.Default()
	if c.HeartbeatInterval != 0 {
		t.Errorf("HeartbeatInterval must default to 0 (disabled), got %d", c.HeartbeatInterval)
	}
}

// TestHeartbeat_OKWithWhitespaceSuppresses ensures the sentinel match trims
// surrounding whitespace — a model may emit " HEARTBEAT_OK\n" and the
// suppression must still fire.
func TestHeartbeat_OKWithWhitespaceSuppresses(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	a.finishTurn(context.Background(), "  HEARTBEAT_OK\n", nil, nil, HeartbeatWakeMessage)

	if len(rep.said) != 0 {
		t.Fatalf("HEARTBEAT_OK with whitespace must still be suppressed; Say called %d time(s)", len(rep.said))
	}
}

// TestHeartbeat_OKNotSuppressedOnNormalTurn ensures the suppression is specific
// to the HEARTBEAT_OK sentinel — a normal turn (not a heartbeat wake) that
// happens to mention "heartbeat" in the answer is still delivered.
func TestHeartbeat_OKNotSuppressedOnNormalTurn(t *testing.T) {
	rep := &captureReporter{}
	a := New(Options{Config: config.Default(), Reporter: rep})

	// A normal user message (not a heartbeat wake) with a non-sentinel answer
	// must be delivered.
	answer := "I've set up a heartbeat monitor for your service."
	a.finishTurn(context.Background(), answer, nil, nil, "set up monitoring")

	if len(rep.said) != 1 || rep.said[0] != answer {
		t.Errorf("non-sentinel answer on a normal turn must be delivered; got %v", rep.said)
	}
}

// TestIsHeartbeatWake verifies the detection of a Chronos heartbeat wake_message:
// the input must contain the HEARTBEAT marker.
func TestIsHeartbeatWake(t *testing.T) {
	if !isHeartbeatWake(HeartbeatWakeMessage) {
		t.Error("the canonical wake_message must be detected as a heartbeat wake")
	}
	if !isHeartbeatWake("HEARTBEAT: review active goals") {
		t.Error("a message containing the HEARTBEAT marker must be detected")
	}
	if isHeartbeatWake("what's the weather?") {
		t.Error("a normal user message must NOT be detected as a heartbeat wake")
	}
	if isHeartbeatWake("") {
		t.Error("empty input must not be a heartbeat wake")
	}
}

// TestShouldSuppressHeartbeat pins the sentinel match: only the exact
// HEARTBEAT_OK token (after trimming) triggers suppression.
func TestShouldSuppressHeartbeat(t *testing.T) {
	if !shouldSuppressHeartbeat(HeartbeatOK) {
		t.Error("exact HEARTBEAT_OK must suppress")
	}
	if !shouldSuppressHeartbeat(" HEARTBEAT_OK ") {
		t.Error("whitespace-padded HEARTBEAT_OK must suppress")
	}
	if shouldSuppressHeartbeat("HEARTBEAT_OK and something else") {
		t.Error("a message containing but not equal to HEARTBEAT_OK must NOT suppress")
	}
	if shouldSuppressHeartbeat("heartbeat_ok") {
		t.Error("case must matter — lowercase must NOT suppress")
	}
	if shouldSuppressHeartbeat("") {
		t.Error("empty answer must not suppress")
	}
}

// TestHeartbeatSentinelsAreDistinct ensures the wake marker and the OK
// sentinel are different strings (a wake marker must not BE the suppression
// sentinel). The wake_message itself may mention HEARTBEAT_OK (it instructs
// the agent to reply with it), but the MARKER used to detect a wake is
// distinct from the sentinel, so a wake input is never mistaken for a
// suppression answer.
func TestHeartbeatSentinelsAreDistinct(t *testing.T) {
	if HeartbeatWakeMarker == HeartbeatOK {
		t.Fatal("wake marker and OK sentinel must be distinct strings")
	}
	// The marker used to detect a heartbeat wake must NOT itself be the
	// suppression sentinel — otherwise the wake input would match the
	// suppression check. The marker is "HEARTBEAT"; the sentinel is
	// "HEARTBEAT_OK" — distinct.
	if shouldSuppressHeartbeat(HeartbeatWakeMarker) {
		t.Fatal("the wake marker must not trigger suppression (it is not the sentinel)")
	}
}
