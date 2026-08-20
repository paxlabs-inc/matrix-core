package effect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

func TestProposal_RejectsInvalidIdentityBoundsAndDigests(t *testing.T) {
	valid := testProposal()
	cases := []Proposal{
		func() Proposal { value := valid; value.ID = ""; return value }(),
		func() Proposal { value := valid; value.OrganizationID = ""; return value }(),
		func() Proposal { value := valid; value.IntentID = ""; return value }(),
		func() Proposal { value := valid; value.NodeID = ""; return value }(),
		func() Proposal { value := valid; value.SeatID = ""; return value }(),
		func() Proposal { value := valid; value.LeaseID = ""; return value }(),
		func() Proposal { value := valid; value.Provider = ""; return value }(),
		func() Proposal { value := valid; value.SkillID = ""; return value }(),
		func() Proposal { value := valid; value.EffectClass = "other"; return value }(),
		func() Proposal { value := valid; value.Irreversible = true; return value }(),
		func() Proposal {
			value := valid
			value.ApprovalID = "approval:unexpected"
			value.ApprovalCost = 1
			return value
		}(),
		func() Proposal { value := valid; value.Operation = ""; return value }(),
		func() Proposal { value := valid; value.IdempotencyKey = ""; return value }(),
		func() Proposal { value := valid; value.Fence = 0; return value }(),
		func() Proposal {
			value := valid
			value.SkillDigest.Algorithm = "bad"
			return value
		}(),
		func() Proposal {
			value := valid
			value.OperationDigest.Algorithm = "bad"
			return value
		}(),
		func() Proposal { value := valid; value.Input = nil; return value }(),
		func() Proposal { value := valid; value.Input = make([]byte, 256<<10+1); return value }(),
		func() Proposal { value := valid; value.Deadline = time.Time{}; return value }(),
	}
	for index, candidate := range cases {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("case %d accepted: %+v", index, candidate)
		}
	}
	badToken := valid
	badToken.ID = "bad token"
	if err := badToken.Validate(); err == nil {
		t.Fatal("invalid token character accepted")
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	irreversible := valid
	irreversible.EffectClass = skills.EffectIrreversible
	irreversible.Irreversible = true
	irreversible.ApprovalID = "approval:exact"
	irreversible.ApprovalCost = 100
	if err := irreversible.Validate(); err != nil {
		t.Fatalf("valid irreversible proposal rejected: %v", err)
	}
	for _, state := range []State{
		StatePrepared, StateDispatching, StateSucceeded, StateFailed,
		StateExternallyAmbiguous,
	} {
		if !state.Valid() {
			t.Fatalf("valid state rejected: %s", state)
		}
	}
	if State("unknown").Valid() {
		t.Fatal("unknown state accepted")
	}
}

func TestCommandAdapter_ExecutesFixedDispatchAndProbeAndBoundsOutput(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	adapter, err := NewCommandAdapter(
		"filesystem", "/bin/sh",
		map[string][]string{
			"write": {"-c", `printf '%s' "$WORKFORCE_IDEMPOTENCY_KEY"; cat`},
			"fail":  {"-c", `printf failed; exit 7`},
			"large": {"-c", `head -c 262145 /dev/zero`},
			"empty": {"-c", `true`},
		},
		map[string][]string{
			"write":     {"-c", `printf '{"outcome":"completed_out_of_band","external_id":"provider:id","observation":"observed"}'`},
			"missing":   {"-c", `printf '{"outcome":"completed_out_of_band"}'`},
			"invalid":   {"-c", `printf '{"outcome":"other"}'`},
			"unchanged": {"-c", `printf '{"outcome":"unchanged","reason":"same"}'`},
		},
		[]string{"PATH=/usr/bin:/bin"}, directory, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Dispatch(context.Background(), Operation{
		Name: "write", IdempotencyKey: "key:test", Input: []byte("-payload"),
	})
	if err != nil || !result.Started ||
		string(result.Observation) != "key:test-payload" ||
		result.ExternalID == "" || !result.ObservedAt.Equal(now) {
		t.Fatalf("dispatch result = %+v, %v", result, err)
	}
	probe, err := adapter.Probe(context.Background(), Operation{Name: "write", IdempotencyKey: "key:test"})
	if err != nil || probe.Outcome != skills.ProbeCompletedOutOfBand ||
		string(probe.Dispatch.Observation) != `"observed"` ||
		probe.Dispatch.ExternalID != "provider:id" {
		t.Fatalf("probe = %+v, %v", probe, err)
	}
	if missing, err := adapter.Probe(context.Background(), Operation{
		Name: "missing", IdempotencyKey: "key:missing",
	}); err == nil || missing.Reason != "probe_missing_evidence" {
		t.Fatalf("missing probe evidence = %+v, %v", missing, err)
	}
	if invalid, err := adapter.Probe(context.Background(), Operation{
		Name: "invalid", IdempotencyKey: "key:invalid",
	}); err == nil || invalid.Reason != "probe_invalid_response" {
		t.Fatalf("invalid probe outcome = %+v, %v", invalid, err)
	}
	if unchanged, err := adapter.Probe(context.Background(), Operation{
		Name: "unchanged", IdempotencyKey: "key:unchanged",
	}); err != nil || unchanged.Dispatch.Started ||
		unchanged.Outcome != skills.ProbeUnchanged {
		t.Fatalf("unchanged probe = %+v, %v", unchanged, err)
	}
	failed, err := adapter.Dispatch(context.Background(), Operation{Name: "fail", IdempotencyKey: "key:fail"})
	if err == nil || !failed.Started {
		t.Fatalf("failed process = %+v, %v", failed, err)
	}
	large, err := adapter.Dispatch(context.Background(), Operation{Name: "large", IdempotencyKey: "key:large"})
	if err == nil || !large.Started || len(large.Observation) != commandOutputLimit {
		t.Fatalf("large output = %d, %+v, %v", len(large.Observation), large, err)
	}
	before, err := adapter.Dispatch(context.Background(), Operation{Name: "missing", IdempotencyKey: "key"})
	if err == nil || before.Started {
		t.Fatalf("unknown operation = %+v, %v", before, err)
	}
	empty, err := adapter.Dispatch(context.Background(), Operation{Name: "empty", IdempotencyKey: "key:empty"})
	if err != nil || string(empty.Observation) != "completed" {
		t.Fatalf("empty observation normalization = %+v, %v", empty, err)
	}
	if _, err := NewCommandAdapter(
		"missing", "/definitely/not/a/real/executable",
		map[string][]string{"op": {"arg"}}, map[string][]string{"op": {"arg"}},
		nil, "", func() time.Time { return now },
	); err == nil {
		t.Fatal("missing executable accepted at construction")
	}
	var buffer limitedBuffer
	_, _ = buffer.Write(make([]byte, commandOutputLimit-3))
	written, err := buffer.Write([]byte("overflow"))
	if err != nil || written != len("overflow") || !buffer.exceeded ||
		len(buffer.Bytes()) != commandOutputLimit {
		t.Fatalf("partial buffer overflow = %d, %v, len=%d, exceeded=%v",
			written, err, len(buffer.Bytes()), buffer.exceeded)
	}
	buffer = limitedBuffer{}
	_, _ = buffer.Write(make([]byte, commandOutputLimit))
	written, err = buffer.Write([]byte("overflow"))
	if err != nil || written != len("overflow") || !buffer.exceeded {
		t.Fatalf("full buffer overflow = %d, %v, exceeded=%v", written, err, buffer.exceeded)
	}
}

func TestCommandAdapter_PinsExecutableAndWorkingDirectoryDescriptors(t *testing.T) {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	root := t.TempDir()
	executable := filepath.Join(root, "provider")
	source, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, source, 0755); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "session")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewCommandAdapter(
		"pinned", executable,
		map[string][]string{"write": {"-c", `pwd; printf pinned`}},
		map[string][]string{"write": {"-c", `printf '{"outcome":"unchanged"}'`}},
		[]string{"PATH=/usr/bin:/bin"}, directory, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(executable, executable+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf replaced\n"), 0755); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Dispatch(context.Background(), Operation{
		Name: "write", IdempotencyKey: "pinned:test",
	})
	if err != nil || !result.Started {
		t.Fatalf("pinned dispatch = %+v, %v", result, err)
	}
	lines := strings.Split(string(result.Observation), "\n")
	if len(lines) != 2 || lines[0] != directory || lines[1] != "pinned" {
		t.Fatalf("descriptor-pinned observation = %q", result.Observation)
	}
	replacedDirectory := directory + ".original"
	if err := os.Rename(directory, replacedDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	rejected, err := adapter.Dispatch(context.Background(), Operation{
		Name: "write", IdempotencyKey: "pinned:replaced-directory",
	})
	if err == nil || rejected.Started {
		t.Fatalf("replaced provider directory = %+v, %v", rejected, err)
	}
}

func TestCommandAdapter_RejectsUnsafeConstructionAndInvalidObservationClock(t *testing.T) {
	validOperations := map[string][]string{"op": {"-c", "true"}}
	validProbes := map[string][]string{"op": {"-c", "true"}}
	cases := []struct {
		name, executable, directory string
		operations, probes          map[string][]string
		now                         func() time.Time
	}{
		{"", "/bin/sh", "", validOperations, validProbes, time.Now},
		{"adapter", "sh", "", validOperations, validProbes, time.Now},
		{"adapter", "/bin/sh", "", nil, validProbes, time.Now},
		{"adapter", "/bin/sh", "", validOperations, nil, time.Now},
		{"adapter", "/bin/sh", "relative", validOperations, validProbes, time.Now},
		{"adapter", "/bin/sh", "", map[string][]string{"bad op": {"x"}}, validProbes, time.Now},
		{"adapter", "/bin/sh", "", map[string][]string{"op": nil}, validProbes, time.Now},
		{"adapter", "/bin/sh", "", map[string][]string{"op": {strings.Repeat("x", 4097)}}, validProbes, time.Now},
		{"adapter", "/bin/sh", "", validOperations, map[string][]string{"bad op": {"x"}}, time.Now},
		{"adapter", "/bin/sh", "", validOperations, validProbes, nil},
	}
	for index, candidate := range cases {
		if _, err := NewCommandAdapter(candidate.name, candidate.executable,
			candidate.operations, candidate.probes, nil, candidate.directory,
			candidate.now); err == nil {
			t.Fatalf("constructor case %d accepted", index)
		}
	}
	for index, environment := range [][]string{
		{"MISSING_SEPARATOR"},
		{"9INVALID=value"},
		{"DUPLICATE=first", "DUPLICATE=second"},
		{"WORKFORCE_IDEMPOTENCY_KEY=caller-controlled"},
		{"LD_PRELOAD=/tmp/provider-override.so"},
		{"BASH_ENV=/tmp/provider-override"},
		{"PATH=/tmp/provider-bin:/usr/bin"},
		{"VALUE=" + strings.Repeat("x", 4097)},
	} {
		if _, err := NewCommandAdapter(
			"environment", "/bin/sh", validOperations, validProbes,
			environment, "", time.Now,
		); err == nil {
			t.Fatalf("constructor environment case %d accepted", index)
		}
	}
	unsafeDirectory := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(unsafeDirectory, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDirectory, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCommandAdapter(
		"directory", "/bin/sh", validOperations, validProbes,
		nil, unsafeDirectory, time.Now,
	); err == nil {
		t.Fatal("constructor accepted a world-writable working directory")
	}
	adapter, err := NewCommandAdapter(
		"clock", "/bin/sh", validOperations, validProbes,
		[]string{"PATH=/usr/bin:/bin"}, "", time.Now,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Dispatch(context.Background(), Operation{Name: "op", IdempotencyKey: "key"})
	if err != nil || !result.Started || result.ObservedAt.Location() == time.UTC {
		t.Fatalf("non-UTC observation clock = %+v, %v", result, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = adapter.Dispatch(cancelled, Operation{Name: "op", IdempotencyKey: "key"})
	if err == nil {
		t.Fatalf("cancelled command succeeded: %+v", result)
	}
	if _, statErr := os.Stat(filepath.Clean("/bin/sh")); statErr != nil {
		t.Fatal(statErr)
	}
	if !errors.Is(ErrAmbiguous, ErrAmbiguous) || strings.TrimSpace(adapter.Name()) != "clock" {
		t.Fatal("adapter identity or error taxonomy changed")
	}
}

func testProposal() Proposal {
	now := time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("a", 64)}
	return Proposal{
		ID: "proposal:test", OrganizationID: "organization:test",
		IntentID: "intent:test", NodeID: "node:test",
		SeatID: "seat:test", LeaseID: "lease:test",
		Fence: 1, Provider: "filesystem", Operation: "write",
		SkillID: "skill:test", EffectClass: skills.EffectReversible,
		IdempotencyKey: "effect:test", SkillDigest: hash,
		OperationDigest: hash, Input: []byte("input"), Deadline: now.Add(time.Hour),
	}
}
