package forgeutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Run executes forge with the given args under root.
func Run(ctx context.Context, forgePath, root string, args ...string) (stdout, stderr string, err error) {
	if forgePath == "" {
		forgePath = "forge"
	}
	// forge v1.7+: --root follows the subcommand (forge build --root PATH).
	full := append(append([]string{}, args...), "--root", root)
	cmd := exec.CommandContext(ctx, forgePath, full...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	return outBuf.String(), errBuf.String(), runErr
}

// RunWithTimeout wraps Run with a default timeout.
func RunWithTimeout(forgePath, root string, timeout time.Duration, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return Run(ctx, forgePath, root, args...)
}

// FormatForgeError combines stderr/stdout for agent-facing messages.
func FormatForgeError(stdout, stderr string, err error) string {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = strings.TrimSpace(stdout)
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return msg
}

// EnsureForge checks forge is callable.
func EnsureForge(forgePath string) error {
	_, stderr, err := RunWithTimeout(forgePath, ".", 10*time.Second, "--version")
	if err != nil {
		return fmt.Errorf("forge unavailable: %s", FormatForgeError("", stderr, err))
	}
	return nil
}
