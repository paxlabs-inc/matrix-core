// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package runtime

import (
	"encoding/json"

	"matrix/executor/tool"
)

// marshalToolResult produces the deterministic JSON byte slice that the
// walker stores on PlanStepBody.Result. Shape mirrors
// cmd/mcl-e2e/walk.go:175-180 so harness consumers and production
// consumers see identical bytes.
func marshalToolResult(res *tool.Result, resultText string) []byte {
	if res == nil {
		return nil
	}
	out, _ := json.Marshal(map[string]interface{}{
		"call_id":     res.CallID,
		"is_error":    res.IsError,
		"duration_ms": res.DurationMs,
		"text":        resultText,
	})
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
