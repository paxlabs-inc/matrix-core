// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import "testing"

func TestResolveDataRootOverridePreservesEntrypointCompatibility(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		dataRoot   string
		cortexRoot string
		want       string
	}{
		{name: "configured", configured: "/configured", want: "/configured"},
		{name: "legacy entrypoint", configured: "/configured", cortexRoot: "/data/cortex", want: "/data/cortex"},
		{name: "canonical wins", configured: "/configured", dataRoot: "/data/new", cortexRoot: "/data/old", want: "/data/new"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveDataRootOverride(
				test.configured, test.dataRoot, test.cortexRoot,
			); got != test.want {
				t.Fatalf("data root = %q, want %q", got, test.want)
			}
		})
	}
}
