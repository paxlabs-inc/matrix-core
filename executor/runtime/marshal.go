// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"encoding/json"

	"centra/executor/tool"
)

// marshalToolResult produces the deterministic JSON byte slice that the
// walker stores on PlanStepBody.Result. Shape mirrors
// cmd/mcl-e2e/walk.go:175-180 so harness consumers and production
// consumers see identical bytes.
func marshalToolResult(res *tool.Result, resultText string) []byte {
	if res == nil {
		return nil
	}
	shape := map[string]interface{}{
		"call_id":     res.CallID,
		"is_error":    res.IsError,
		"duration_ms": res.DurationMs,
		"text":        resultText,
	}
	if res.FailureClass != tool.FailureNone {
		shape["failure_class"] = res.FailureClass
	}
	if res.FailureMessage != "" {
		shape["failure_message"] = res.FailureMessage
	}
	if res.Retryable {
		shape["retryable"] = true
	}
	if res.ProcessExitCode != nil {
		shape["process_exit_code"] = *res.ProcessExitCode
	}
	if res.ProcessTimedOut {
		shape["process_timed_out"] = true
	}
	if res.HTTPStatus != 0 {
		shape["http_status"] = res.HTTPStatus
	}
	if res.ApplicationOK != nil {
		shape["application_ok"] = *res.ApplicationOK
	}
	out, _ := json.Marshal(shape)
	return out
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
