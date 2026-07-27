//go:build linux

package tool

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestConfigureAgentCommandDropsIdentityAndBlocksParentProcfs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("credential transition requires root")
	}
	t.Setenv("MATRIX_AGENT_UID", "65534")
	t.Setenv("MATRIX_AGENT_GID", "65534")
	t.Setenv("TAVILY_API_KEY", "parent-secret-sentinel")
	t.Setenv("UNREVIEWED_TOKEN", "unknown-secret-sentinel")

	cmd := exec.Command("sh", "-c", `printf '%s|%s|' "$(id -u)" "$HOME"; env; cat /proc/$PPID/environ`)
	cmd.Env = AgentEnvironment(os.Environ())
	if err := ConfigureAgentCommand(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("unprivileged child unexpectedly read its root parent's environment")
	}
	text := string(out)
	if !strings.HasPrefix(text, "65534|/home/matrix-agent|") {
		t.Fatalf("child identity was not dropped: %q", text)
	}
	for _, sentinel := range []string{"parent-secret-sentinel", "unknown-secret-sentinel"} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("child output leaked %s", sentinel)
		}
	}
}

func TestAgentProcessExfiltrationMatrix(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("credential transition requires root")
	}
	t.Setenv("MATRIX_AGENT_UID", "65534")
	t.Setenv("MATRIX_AGENT_GID", "65534")
	t.Setenv("TAVILY_API_KEY", "matrix-protected-process-sentinel")
	t.Setenv("UNREVIEWED_TOKEN", "matrix-unknown-process-sentinel")
	t.Setenv("MATRIX_USER_ID", "matrix-visible-process-sentinel")

	sibling := exec.Command("sleep", "30")
	sibling.Env = os.Environ()
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sibling.Process.Kill()
		_ = sibling.Wait()
	}()

	script := `
printf 'uid=%s home=%s\n' "$(id -u)" "$HOME"
env
printenv
printf 'expanded=%s|%s\n' "$TAVILY_API_KEY" "$UNREVIEWED_TOKEN"
tr '\000' '\n' </proc/self/environ
for target in 1 "$PPID" "$1"; do
	if cat "/proc/$target/environ" 2>/dev/null; then
		printf '\nproc-%s-environ-readable\n' "$target"
	else
		printf 'proc-%s-environ-denied\n' "$target"
	fi
done
tr '\000' ' ' <"/proc/$1/cmdline"
`
	cmd := exec.Command("sh", "-c", script, "agent-probe", strconv.Itoa(sibling.Process.Pid))
	cmd.Env = AgentEnvironment(os.Environ())
	if err := ConfigureAgentCommand(cmd); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("agent probe failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	text := string(out)
	if !strings.Contains(text, "uid=65534 home=/home/matrix-agent") {
		t.Fatalf("agent identity was not dropped: %q", text)
	}
	if !strings.Contains(text, "MATRIX_USER_ID=matrix-visible-process-sentinel") {
		t.Fatalf("approved configuration missing from agent process: %q", text)
	}
	for _, sentinel := range []string{
		"matrix-protected-process-sentinel",
		"matrix-unknown-process-sentinel",
	} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("agent exfiltration probe leaked %q", sentinel)
		}
	}
	for _, target := range []string{"1", strconv.Itoa(os.Getpid()), strconv.Itoa(sibling.Process.Pid)} {
		if !strings.Contains(text, "proc-"+target+"-environ-denied") {
			t.Fatalf("agent could read protected process %s environment: %q", target, text)
		}
	}
	if !strings.Contains(text, "sleep 30") {
		t.Fatalf("sibling command line probe did not execute: %q", text)
	}
}

func TestDisableProcessDumpSetsKernelFlag(t *testing.T) {
	if os.Getenv("MATRIX_DUMPABLE_HELPER") == "1" {
		if err := DisableProcessDump(); err != nil {
			os.Exit(2)
		}
		value, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
		if err != nil || value != 0 {
			os.Exit(3)
		}
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestDisableProcessDumpSetsKernelFlag")
	cmd.Env = append(os.Environ(), "MATRIX_DUMPABLE_HELPER=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dumpable helper failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

func TestAgentIdentityRejectsRootAndPartialConfiguration(t *testing.T) {
	for _, tc := range []struct {
		uid string
		gid string
	}{
		{uid: "0", gid: "10001"},
		{uid: "10001", gid: "0"},
		{uid: "not-a-number", gid: "10001"},
	} {
		t.Run(tc.uid+"-"+tc.gid, func(t *testing.T) {
			t.Setenv("MATRIX_AGENT_UID", tc.uid)
			t.Setenv("MATRIX_AGENT_GID", tc.gid)
			if identity, configured, err := AgentIdentityFromEnv(); err == nil {
				t.Fatalf("invalid identity accepted: %+v configured=%v", identity, configured)
			}
		})
	}

	t.Setenv("MATRIX_AGENT_UID", strconv.Itoa(10001))
	t.Setenv("MATRIX_AGENT_GID", strconv.Itoa(10001))
	identity, configured, err := AgentIdentityFromEnv()
	if err != nil || !configured || identity.UID != 10001 || identity.GID != 10001 {
		t.Fatalf("valid identity rejected: %+v configured=%v err=%v", identity, configured, err)
	}
}
