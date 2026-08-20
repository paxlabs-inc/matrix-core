// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"strings"
	"testing"

	"centra/executor/tool"
)

func TestMarshalToolResultPreservesSuccessShapeAndAddsFailureLayers(t *testing.T) {
	success := &tool.Result{CallID: "call", DurationMs: 12}
	if got, want := string(marshalToolResult(success, "ok")), `{"call_id":"call","duration_ms":12,"is_error":false,"text":"ok"}`; got != want {
		t.Fatalf("successful result shape changed: %s", got)
	}

	exit := 7
	appOK := false
	failure := &tool.Result{
		CallID: "failed", DurationMs: 13, IsError: true,
		FailureClass: tool.FailureApplication, FailureMessage: "rejected",
		ProcessExitCode: &exit, HTTPStatus: 400, ApplicationOK: &appOK,
	}
	got := string(marshalToolResult(failure, "raw evidence"))
	for _, field := range []string{
		`"failure_class":"application"`, `"failure_message":"rejected"`,
		`"process_exit_code":7`, `"http_status":400`, `"application_ok":false`,
		`"text":"raw evidence"`,
	} {
		if !strings.Contains(got, field) {
			t.Fatalf("failed result missing %s: %s", field, got)
		}
	}
}
