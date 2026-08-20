// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"os"
	"os/exec"

	"centra/executor/tool"
)

// newSysCmd is a thin wrapper isolated in its own file so test fakes
// can stub it later without touching the rest of setup.
func newSysCmd(dir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = tool.AgentEnvironment(os.Environ())
	if err := tool.ConfigureAgentCommand(cmd); err != nil {
		cmd.Err = err
	}
	return cmd
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
