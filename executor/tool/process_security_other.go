//go:build !linux

package tool

import (
	"os/exec"
)

type AgentIdentity struct {
	UID  uint32
	GID  uint32
	Home string
	User string
}

func AgentIdentityFromEnv() (AgentIdentity, bool, error) {
	return AgentIdentity{}, false, nil
}

func ConfigureAgentCommand(*exec.Cmd) error {
	return nil
}

func DisableProcessDump() error {
	return nil
}
