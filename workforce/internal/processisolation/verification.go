package processisolation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VerificationSpec is one fixed, read-only workspace verification boundary.
// The executable and arguments are kernel configuration; no agent-controlled
// path, environment, mount, or network access crosses this boundary.
type VerificationSpec struct {
	Bubblewrap string
	Executable string
	Arguments  []string
	Workspace  string
}

// WorkspaceCommand owns the pinned workspace descriptor used by Bubblewrap.
type WorkspaceCommand struct {
	*exec.Cmd
	workspace *os.File
}

// Run executes the verifier and releases the pinned workspace descriptor.
func (command *WorkspaceCommand) Run() error {
	return errors.Join(command.Cmd.Run(), command.Close())
}

// Close releases a verification command that was built but not run.
func (command *WorkspaceCommand) Close() error {
	if command.workspace == nil {
		return nil
	}
	if err := command.workspace.Close(); err != nil {
		return fmt.Errorf("processisolation: release verification workspace: %w", err)
	}
	command.workspace = nil
	return nil
}

// VerificationCommand builds a networkless, capability-free verifier with a
// read-only pinned workspace and a fresh writable cache.
func VerificationCommand(
	ctx context.Context,
	spec VerificationSpec,
) (*WorkspaceCommand, error) {
	bubblewrap, err := absoluteExecutable(spec.Bubblewrap, "bubblewrap")
	if err != nil {
		return nil, err
	}
	if err := verifyTrustedLauncher(bubblewrap); err != nil {
		return nil, err
	}
	executable, err := absoluteExecutable(spec.Executable, "verification executable")
	if err != nil {
		return nil, err
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("processisolation: resolve verification executable: %w", err)
	}
	if err := verifyTrustedProgram(resolvedExecutable); err != nil {
		return nil, err
	}
	sandboxExecutable := resolvedExecutable
	var toolchainRoot string
	if !pathWithin(resolvedExecutable, "/usr") {
		if filepath.Base(resolvedExecutable) != "go" ||
			filepath.Base(filepath.Dir(resolvedExecutable)) != "bin" {
			return nil, fmt.Errorf(
				"processisolation: verification executable is outside the trusted image",
			)
		}
		toolchainRoot = filepath.Dir(filepath.Dir(resolvedExecutable))
		sandboxExecutable = "/toolchain/bin/go"
	}
	if len(spec.Arguments) > 32 {
		return nil, fmt.Errorf("processisolation: verification arguments exceed bound")
	}
	for _, argument := range spec.Arguments {
		if len(argument) > 4096 || strings.IndexByte(argument, 0) >= 0 {
			return nil, fmt.Errorf("processisolation: invalid verification argument")
		}
	}
	root, err := filepath.Abs(spec.Workspace)
	if err != nil {
		return nil, fmt.Errorf("processisolation: resolve verification workspace: %w", err)
	}
	declared, err := os.Lstat(root)
	if err != nil || declared.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("processisolation: verification workspace is invalid")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("processisolation: resolve verification workspace: %w", err)
	}
	before, err := os.Lstat(root)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("processisolation: verification workspace is invalid")
	}
	workspace, err := os.Open(root)
	if err != nil {
		return nil, fmt.Errorf("processisolation: open verification workspace: %w", err)
	}
	release := true
	defer func() {
		if release {
			_ = workspace.Close()
		}
	}()
	opened, err := workspace.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("processisolation: verification workspace changed while opening")
	}
	args := []string{
		"--unshare-all", "--unshare-user", "--disable-userns",
		"--die-with-parent", "--new-session",
		"--cap-drop", "ALL", "--uid", "65534", "--gid", "65534",
		"--ro-bind-fd", "3", "/workspace",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--tmpfs", "/tmp", "--tmpfs", "/cache",
		"--proc", "/proc", "--dev", "/dev",
		"--chdir", "/workspace", "--clearenv",
		"--setenv", "PATH", "/usr/local/go/bin:/usr/bin",
		"--setenv", "HOME", "/tmp",
		"--setenv", "GOCACHE", "/cache",
		"--setenv", "GOTOOLCHAIN", "local",
		"--setenv", "CGO_ENABLED", "0",
		"--unsetenv", "PWD",
	}
	if toolchainRoot != "" {
		args = append(args, "--ro-bind", toolchainRoot, "/toolchain")
	}
	args = append(args, sandboxExecutable)
	args = append(args, spec.Arguments...)
	command := exec.CommandContext(ctx, bubblewrap, args...)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.ExtraFiles = []*os.File{workspace}
	release = false
	return &WorkspaceCommand{Cmd: command, workspace: workspace}, nil
}

func verifyTrustedProgram(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("processisolation: inspect verification executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 ||
		info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("processisolation: verification executable is replaceable")
	}
	if err := verifyTrustedOwner(info, "verification executable"); err != nil {
		return err
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("processisolation: verification executable ancestry is replaceable")
		}
		if err := verifyTrustedOwner(info, "verification executable path component"); err != nil {
			return err
		}
		if directory == "/" {
			return nil
		}
	}
}

func pathWithin(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
