// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"testing"
	"time"
)

func TestResurrectionSSECommitsOnlyAcceptedAttemptAndMarksSavedPartial(
	t *testing.T,
) {
	engine := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(engine.Close)
	sess := &session{id: "resurrection-conv", engine: engine}
	current := &run{
		id: "resurrection-run", convID: sess.id, sess: sess,
		started: time.Now(),
	}
	sess.cur = current
	engine.broker.ensure(current.id)
	reporter := &sseReporter{sess: sess}

	reporter.Delta(0, "content", "I already provided the answer above.")
	reporter.Delta(0, "retraction", "")
	reporter.Delta(1, "reasoning", "Checking saved evidence. ")
	reporter.Delta(1, "content", "Verified saved work remains.")
	reporter.Delta(1, "commit", "")
	reporter.SayHonestPartial("Verified saved work remains.")

	replay, _, cancel := engine.broker.subscribe(current.id, 0)
	cancel()
	wantTypes := []string{
		"chat.delta",
		"chat.retraction",
		"chat.delta",
		"chat.delta",
		"chat.attempt.commit",
		"message.complete",
		"chat.assistant",
	}
	if len(replay) != len(wantTypes) {
		t.Fatalf("events = %+v, want types %v", replay, wantTypes)
	}
	for index, wantType := range wantTypes {
		if replay[index].Type != wantType {
			t.Fatalf(
				"event %d type = %q, want %q",
				index, replay[index].Type, wantType,
			)
		}
	}
	commit := replay[4]
	if commit.Fields["text"] != "Verified saved work remains." ||
		commit.Fields["reasoning"] != "Checking saved evidence." {
		t.Fatalf("attempt commit = %+v", commit.Fields)
	}
	final := replay[len(replay)-1]
	if final.Fields["honest_partial"] != true ||
		final.Fields["incomplete"] != true ||
		final.Fields["resumable"] != true ||
		final.Fields["text"] != "Verified saved work remains." {
		t.Fatalf("honest partial terminal = %+v", final.Fields)
	}
	history := engine.conv.History(sess.id)
	if len(history) != 1 ||
		history[0].Text != "Verified saved work remains." {
		t.Fatalf("durable accepted content = %+v", history)
	}
}
