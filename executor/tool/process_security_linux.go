//go:build linux

package tool

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

type AgentIdentity struct {
	UID  uint32
	GID  uint32
	Home string
	User string
}

// AgentIdentityFromEnv resolves the production-only unprivileged identity.
// An unset pair preserves local development behavior; a partial or invalid
// pair fails closed.
func AgentIdentityFromEnv() (AgentIdentity, bool, error) {
	uidText, uidSet := os.LookupEnv("MATRIX_AGENT_UID")
	gidText, gidSet := os.LookupEnv("MATRIX_AGENT_GID")
	if !uidSet && !gidSet {
		return AgentIdentity{}, false, nil
	}
	if !uidSet || !gidSet {
		return AgentIdentity{}, false, fmt.Errorf("agent identity requires MATRIX_AGENT_UID and MATRIX_AGENT_GID")
	}
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil || uid == 0 {
		return AgentIdentity{}, false, fmt.Errorf("invalid MATRIX_AGENT_UID")
	}
	gid, err := strconv.ParseUint(gidText, 10, 32)
	if err != nil || gid == 0 {
		return AgentIdentity{}, false, fmt.Errorf("invalid MATRIX_AGENT_GID")
	}
	return AgentIdentity{
		UID: uint32(uid), GID: uint32(gid),
		Home: "/home/matrix-agent", User: "matrix-agent",
	}, true, nil
}

// ConfigureAgentCommand drops one agent-controlled child to the dedicated
// runtime identity and corrects identity-related environment values.
func ConfigureAgentCommand(cmd *exec.Cmd) error {
	identity, configured, err := AgentIdentityFromEnv()
	if err != nil || !configured {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("agent identity configured but parent is not privileged")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: identity.UID, Gid: identity.GID, NoSetGroups: true,
	}
	cmd.Env = setEnvironmentValue(cmd.Env, "HOME", identity.Home)
	cmd.Env = setEnvironmentValue(cmd.Env, "USER", identity.User)
	cmd.Env = setEnvironmentValue(cmd.Env, "LOGNAME", identity.User)
	return nil
}

// DisableProcessDump prevents same-UID ptrace and procfs memory/environment
// inspection of a secret-holding service process.
func DisableProcessDump() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("disable process dump: %w", err)
	}
	return nil
}

func setEnvironmentValue(env []string, key, value string) []string {
	prefix := key + "="
	for i := range env {
		if len(env[i]) >= len(prefix) && env[i][:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
