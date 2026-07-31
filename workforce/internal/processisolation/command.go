// Package processisolation owns the common operating-system boundary for
// short-lived Workforce seat and Auditor processes.
package processisolation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Spec struct {
	Bubblewrap          string
	Binary              string
	ExpectedBuildDigest string
	Target              string
	Env                 map[string]string
}

var allowedEnvironment = map[string]struct{}{
	"WORKFORCE_SESSION":                    {},
	"WORKFORCE_DEVELOPER_AUDIT_KEY_ID":     {},
	"WORKFORCE_DEVELOPER_AUDIT_PUBLIC_KEY": {},
}

type IsolatedCommand struct {
	*exec.Cmd
	binary    *os.File
	directory string
}

// Run executes the isolated worker and always releases the verified image.
// Both failures are reported: a leaked image is a real condition, and joining
// keeps the execution error matchable by errors.Is.
func (command *IsolatedCommand) Run() error {
	runErr := command.Cmd.Run()
	return errors.Join(runErr, command.Close())
}

// Close releases the private verified worker image. Run calls it; callers that
// abandon a built command before running it must call it themselves. Each piece
// of state is cleared only once it is actually released, so a failed cleanup
// stays visible to a repeated Close rather than being silently forgotten.
func (command *IsolatedCommand) Close() error {
	var failures []error
	if command.binary != nil {
		if err := command.binary.Close(); err != nil {
			failures = append(failures, err)
		} else {
			command.binary = nil
		}
	}
	if command.directory != "" {
		if err := os.RemoveAll(command.directory); err != nil {
			failures = append(failures, err)
		} else {
			command.directory = ""
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("processisolation: release worker image: %w",
			errors.Join(failures...))
	}
	return nil
}

// Command builds the deny-by-default Bubblewrap command for one trusted
// Workforce worker binary. The environment values are trusted kernel inputs;
// the worker process, its output, and all packet content remain untrusted.
func Command(ctx context.Context, spec Spec) (*IsolatedCommand, error) {
	bubblewrap, err := absoluteExecutable(spec.Bubblewrap, "bubblewrap")
	if err != nil {
		return nil, err
	}
	if err := verifyTrustedLauncher(bubblewrap); err != nil {
		return nil, err
	}
	binary, err := absoluteExecutable(spec.Binary, "worker")
	if err != nil {
		return nil, err
	}
	binaryFile, directory, err := openVerifiedExecutable(binary, spec.ExpectedBuildDigest)
	if err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			_ = binaryFile.Close()
			_ = os.RemoveAll(directory)
		}
	}()
	if spec.Target != "/workforce-seat" && spec.Target != "/workforce-auditor" {
		return nil, fmt.Errorf("processisolation: invalid worker target")
	}
	args := []string{
		"--unshare-all", "--unshare-user", "--disable-userns",
		"--die-with-parent", "--new-session",
		"--cap-drop", "ALL", "--uid", "65534", "--gid", "65534",
		"--ro-bind-fd", "3", spec.Target,
		"--tmpfs", "/session", "--chdir", "/session",
		"--proc", "/proc", "--dev", "/dev", "--clearenv",
	}
	keys := make([]string, 0, len(spec.Env))
	for key, value := range spec.Env {
		if !validEnvironmentName(key) || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("processisolation: invalid worker environment")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--setenv", key, spec.Env[key])
	}
	args = append(args, "--unsetenv", "PWD")
	args = append(args, spec.Target)
	command := exec.CommandContext(ctx, bubblewrap, args...)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.ExtraFiles = []*os.File{binaryFile}
	release = false
	return &IsolatedCommand{
		Cmd: command, binary: binaryFile, directory: directory,
	}, nil
}

func absoluteExecutable(value, name string) (string, error) {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
		return "", fmt.Errorf("processisolation: absolute %s path is required", name)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("processisolation: resolve %s path: %w", name, err)
	}
	return absolute, nil
}

// verifyTrustedLauncher rejects a Bubblewrap launcher that anyone outside the
// kernel's own trust boundary could replace. The launcher runs with the
// workforced identity before any sandbox argument takes effect, so a
// substituted launcher would silently ignore the whole boundary.
func verifyTrustedLauncher(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("processisolation: inspect bubblewrap: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 ||
		info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("processisolation: bubblewrap must be a non-writable executable regular file")
	}
	if err := verifyTrustedOwner(info, "bubblewrap"); err != nil {
		return err
	}
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		directoryInfo, err := os.Lstat(directory)
		if err != nil {
			return fmt.Errorf("processisolation: inspect bubblewrap path: %w", err)
		}
		if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("processisolation: bubblewrap path component %q is not a directory", directory)
		}
		if directoryInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("processisolation: bubblewrap path component %q is writable", directory)
		}
		if err := verifyTrustedOwner(directoryInfo, "bubblewrap path component"); err != nil {
			return err
		}
		if directory == "/" {
			return nil
		}
	}
}

// verifyTrustedOwner requires root ownership rather than merely accepting the
// service's own identity. The launcher is executed by pathname, so anything a
// same-identity process could replace between verification and execution would
// silently discard the sandbox.
func verifyTrustedOwner(info os.FileInfo, name string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("processisolation: cannot determine %s ownership", name)
	}
	if stat.Uid != 0 {
		return fmt.Errorf("processisolation: %s is not owned by root", name)
	}
	return nil
}

// openVerifiedExecutable returns a read-only descriptor whose bytes cannot
// change after verification, together with the private directory holding it.
//
// Copying the deployed worker into a private image is what makes verification
// binding: hashing the deployed inode in place would leave a window in which a
// retained writable descriptor could rewrite those bytes after verification and
// before execution. Three properties keep the copy fixed. Its parent must not
// be an unprotected shared directory, so nobody else can rename or replace the
// private directory. The read-only descriptor is reopened through /proc/self/fd
// rather than by path, so the inode is reached from the descriptor already held
// instead of through a second name lookup a rename could redirect. And the only
// writable descriptor is closed before the image is hashed, so from that point
// the bytes behind the returned descriptor cannot be rewritten.
//
// The image stays linked because Bubblewrap binds it by path; an unlinked inode
// cannot be bind-mounted.
func openVerifiedExecutable(path, expectedDigest string) (*os.File, string, error) {
	if len(expectedDigest) != sha256.Size*2 {
		return nil, "", fmt.Errorf("processisolation: expected worker build digest is invalid")
	}
	for _, character := range expectedDigest {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return nil, "", fmt.Errorf("processisolation: expected worker build digest is invalid")
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", fmt.Errorf("processisolation: inspect worker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 ||
		info.Mode().Perm()&0o022 != 0 {
		return nil, "", fmt.Errorf("processisolation: worker must be a non-writable executable regular file")
	}
	source, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("processisolation: open worker: %w", err)
	}
	defer source.Close()
	openedInfo, err := source.Stat()
	if err != nil || !os.SameFile(info, openedInfo) ||
		!openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0o111 == 0 ||
		openedInfo.Mode().Perm()&0o022 != 0 {
		return nil, "", fmt.Errorf("processisolation: worker identity changed while opening")
	}
	if err := verifyPrivateImageParent(os.TempDir()); err != nil {
		return nil, "", err
	}
	directory, err := os.MkdirTemp("", "workforce-worker-")
	if err != nil {
		return nil, "", fmt.Errorf("processisolation: private worker image: %w", err)
	}
	release := true
	defer func() {
		if release {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, "", fmt.Errorf("processisolation: private worker image: %w", err)
	}
	imagePath := filepath.Join(directory, "worker")
	writable, err := os.OpenFile(imagePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o500)
	if err != nil {
		return nil, "", fmt.Errorf("processisolation: create private worker image: %w", err)
	}
	closeWritable := true
	defer func() {
		if closeWritable {
			_ = writable.Close()
		}
	}()
	if _, err := io.Copy(writable, source); err != nil {
		return nil, "", fmt.Errorf("processisolation: copy worker: %w", err)
	}
	file, err := os.Open("/proc/self/fd/" + strconv.Itoa(int(writable.Fd())))
	if err != nil {
		return nil, "", fmt.Errorf("processisolation: reopen private worker image: %w", err)
	}
	writableInfo, statErr := writable.Stat()
	imageInfo, err := file.Stat()
	if statErr != nil || err != nil || !os.SameFile(writableInfo, imageInfo) ||
		!imageInfo.Mode().IsRegular() || imageInfo.Mode().Perm()&0o022 != 0 {
		_ = file.Close()
		return nil, "", fmt.Errorf("processisolation: private worker image identity changed")
	}
	// Dropping the only writable descriptor freezes the image: the descriptor
	// returned below is read-only, and the file's own mode denies further
	// writers.
	closeWritable = false
	if err := writable.Close(); err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("processisolation: seal private worker image: %w", err)
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("processisolation: hash worker: %w", err)
	}
	if hex.EncodeToString(sum.Sum(nil)) != expectedDigest {
		_ = file.Close()
		return nil, "", fmt.Errorf("processisolation: worker build digest does not match lease")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, "", fmt.Errorf("processisolation: rewind worker: %w", err)
	}
	release = false
	return file, directory, nil
}

// verifyPrivateImageParent rejects a temporary parent that other users could
// rename or replace entries in. A shared directory is acceptable only when the
// sticky bit prevents one user from removing another's entries, which is what
// keeps the private image directory ours for its whole lifetime.
func verifyPrivateImageParent(parent string) error {
	// Lstat, not Stat: a symlinked temporary root would otherwise be validated
	// through its target while creation and cleanup follow the link somewhere
	// else entirely.
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("processisolation: inspect worker image parent: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("processisolation: worker image parent %q is not a directory", parent)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("processisolation: cannot determine worker image parent ownership")
	}
	if stat.Uid != 0 && uint64(stat.Uid) != uint64(os.Geteuid()) {
		return fmt.Errorf("processisolation: worker image parent %q is owned by another user", parent)
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("processisolation: worker image parent %q is shared and not sticky", parent)
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if _, allowed := allowedEnvironment[value]; !allowed {
		return false
	}
	if value == "" || strings.ContainsAny(value, "=\x00\r\n") {
		return false
	}
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}
