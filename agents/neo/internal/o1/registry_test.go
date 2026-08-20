// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestShippedOperationsHaveClassifiedHazards(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	registry, err := Load(filepath.Join(root, "protocol", "spec", "architect-o1", "hazards.json"))
	if err != nil {
		t.Fatal(err)
	}
	operations, err := ManifestOperations(filepath.Join(root, "agents", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Check(operations); err != nil {
		t.Fatal(err)
	}
}
