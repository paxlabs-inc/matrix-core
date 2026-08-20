// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import "testing"

func TestBuildProjectOwnsProjectSelectionSchema(t *testing.T) {
	buildProperties := buildProjectSchema().Function.Parameters["properties"].(map[string]interface{})
	if _, ok := buildProperties["project"]; !ok {
		t.Fatal("build_project does not expose its project admission field")
	}
	coreProperties := coreExecuteSchema().Function.Parameters["properties"].(map[string]interface{})
	if _, ok := coreProperties["project"]; ok {
		t.Fatal("core_execute advertises the Build-only project field")
	}
}
