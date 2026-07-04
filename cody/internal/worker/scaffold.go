// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"os"
	"sort"
	"strings"
)

// ScaffoldStacks lists the stacks the scaffolder suite at dir supports, from
// its scaffold-<stack>.sh files. Empty when the dir is unset or unreadable —
// the prompt section is simply omitted then (boot-safe).
func ScaffoldStacks(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "scaffold-") || !strings.HasSuffix(name, ".sh") {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "scaffold-"), ".sh"))
	}
	sort.Strings(out)
	return out
}
