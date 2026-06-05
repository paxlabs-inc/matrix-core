// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"os/exec"
)

// newSysCmd is a thin wrapper isolated in its own file so test fakes
// can stub it later without touching the rest of setup.
func newSysCmd(dir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
