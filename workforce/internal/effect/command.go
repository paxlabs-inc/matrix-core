package effect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"centra/workforce/internal/skills"
)

const commandOutputLimit = 256 << 10

// CommandAdapter executes a fixed allowlisted subprocess without exposing its
// credential environment to seat processes.
type CommandAdapter struct {
	name             string
	executable       *os.File
	executableDigest [sha256.Size]byte
	operations       map[string][]string
	probes           map[string][]string
	environment      []string
	directory        *os.File
	directoryPath    string
	now              func() time.Time
}

// NewCommandAdapter constructs a real fixed-command provider adapter.
func NewCommandAdapter(
	name, executable string,
	operations, probes map[string][]string,
	environment []string,
	directory string,
	now func() time.Time,
) (*CommandAdapter, error) {
	if err := validateToken("adapter name", name); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(executable) || len(operations) == 0 || len(probes) == 0 || now == nil {
		return nil, fmt.Errorf("command adapter requires an absolute executable, operations, probes, and time source")
	}
	if directory != "" && !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("command adapter directory must be absolute")
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("command adapter requires Linux descriptor execution")
	}
	for operation, arguments := range operations {
		if err := validateCommand(operation, arguments); err != nil {
			return nil, err
		}
	}
	for operation, arguments := range probes {
		if err := validateCommand(operation, arguments); err != nil {
			return nil, err
		}
	}
	if err := validateEnvironment(environment); err != nil {
		return nil, err
	}
	executableFile, executableDigest, err := openPinnedExecutable(executable)
	if err != nil {
		return nil, err
	}
	var directoryFile *os.File
	if directory != "" {
		directoryFile, err = openPinnedDirectory(directory)
		if err != nil {
			_ = executableFile.Close()
			return nil, err
		}
	}
	return &CommandAdapter{
		name: name, executable: executableFile, executableDigest: executableDigest,
		operations: cloneCommands(operations),
		probes:     cloneCommands(probes), environment: append([]string(nil), environment...),
		directory: directoryFile, directoryPath: directory, now: now,
	}, nil
}

// Name returns the stable provider identity.
func (adapter *CommandAdapter) Name() string { return adapter.name }

// Dispatch starts one allowlisted external mutation.
func (adapter *CommandAdapter) Dispatch(
	ctx context.Context,
	operation Operation,
) (DispatchResult, error) {
	return adapter.run(ctx, operation, adapter.operations)
}

// Probe performs one allowlisted read-only authoritative observation.
func (adapter *CommandAdapter) Probe(
	ctx context.Context,
	operation Operation,
) (ProbeResult, error) {
	result, err := adapter.run(ctx, operation, adapter.probes)
	if err != nil {
		return ProbeResult{
			Outcome: skills.ProbeUnknown, Dispatch: result, Reason: "probe_process_failed",
		}, err
	}
	var envelope struct {
		Outcome     skills.ProbeOutcome `json:"outcome"`
		ExternalID  string              `json:"external_id"`
		Observation json.RawMessage     `json:"observation"`
		Reason      string              `json:"reason"`
	}
	if err := json.Unmarshal(result.Observation, &envelope); err != nil ||
		!envelope.Outcome.Valid() {
		return ProbeResult{
			Outcome: skills.ProbeUnknown, Dispatch: result, Reason: "probe_invalid_response",
		}, fmt.Errorf("probe_invalid_response")
	}
	if strings.TrimSpace(envelope.ExternalID) != "" {
		result.ExternalID = envelope.ExternalID
	}
	result.Observation = append([]byte(nil), envelope.Observation...)
	switch envelope.Outcome {
	case skills.ProbeCompletedOutOfBand:
		if len(result.Observation) == 0 || strings.TrimSpace(result.ExternalID) == "" {
			return ProbeResult{
				Outcome: skills.ProbeUnknown, Dispatch: result, Reason: "probe_missing_evidence",
			}, fmt.Errorf("probe_missing_evidence")
		}
	default:
		result.Started = false
	}
	return ProbeResult{
		Outcome: envelope.Outcome, Dispatch: result, Reason: strings.TrimSpace(envelope.Reason),
	}, nil
}

func (adapter *CommandAdapter) run(
	ctx context.Context,
	operation Operation,
	commands map[string][]string,
) (DispatchResult, error) {
	arguments, ok := commands[operation.Name]
	if !ok {
		return DispatchResult{}, fmt.Errorf("operation_not_allowed")
	}
	currentDigest, err := hashOpenedFile(adapter.executable)
	if err != nil || currentDigest != adapter.executableDigest {
		return DispatchResult{Started: false}, fmt.Errorf("provider_executable_changed")
	}
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.ExtraFiles = []*os.File{adapter.executable}
	if adapter.directory != nil {
		current, statErr := os.Stat(adapter.directoryPath)
		opened, openedErr := adapter.directory.Stat()
		if statErr != nil || openedErr != nil || !os.SameFile(current, opened) {
			return DispatchResult{Started: false}, fmt.Errorf("provider_directory_changed")
		}
		command.Dir = fmt.Sprintf(
			"/proc/%d/fd/%d", os.Getpid(), adapter.directory.Fd(),
		)
	}
	command.Env = append(append([]string(nil), adapter.environment...),
		"WORKFORCE_IDEMPOTENCY_KEY="+operation.IdempotencyKey)
	command.Stdin = bytes.NewReader(operation.Input)
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return DispatchResult{Started: false}, fmt.Errorf("provider_start_failed")
	}
	err = command.Wait()
	result := DispatchResult{
		Started: true, ExternalID: externalID(adapter.name, operation.IdempotencyKey),
		Observation: append([]byte(nil), output.Bytes()...), ObservedAt: adapter.now(),
	}
	if output.exceeded {
		return result, fmt.Errorf("provider_output_exceeded")
	}
	if err != nil {
		return result, fmt.Errorf("provider_process_failed")
	}
	if len(result.Observation) == 0 {
		result.Observation = []byte("completed")
	}
	return result, nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	if buffer.buffer.Len()+len(value) > commandOutputLimit {
		remaining := commandOutputLimit - buffer.buffer.Len()
		if remaining > 0 {
			_, _ = buffer.buffer.Write(value[:remaining])
		}
		buffer.exceeded = true
		return len(value), nil
	}
	return buffer.buffer.Write(value)
}

func (buffer *limitedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }

func openPinnedExecutable(path string) (*os.File, [sha256.Size]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("command adapter executable: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("command adapter executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 ||
		info.Mode().Perm()&0022 != 0 {
		return nil, [sha256.Size]byte{}, fmt.Errorf("command adapter executable must be a non-writable regular executable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return nil, [sha256.Size]byte{}, fmt.Errorf("command adapter executable must be owned by root")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("command adapter executable: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, [sha256.Size]byte{}, fmt.Errorf("command adapter executable changed while opening")
	}
	digest, err := hashOpenedFile(file)
	if err != nil {
		_ = file.Close()
		return nil, [sha256.Size]byte{}, err
	}
	return file, digest, nil
}

func openPinnedDirectory(path string) (*os.File, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("command adapter directory: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, fmt.Errorf("command adapter directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("command adapter directory must be a directory")
	}
	if info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("command adapter directory must not be group or world writable")
	}
	if err := validateTrustedDirectoryAncestry(resolved); err != nil {
		return nil, err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("command adapter directory: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("command adapter directory changed while opening")
	}
	return file, nil
}

func validateTrustedDirectoryAncestry(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("command adapter directory ancestry: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return fmt.Errorf("command adapter directory ancestry must be owned by root")
		}
		if info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("command adapter directory ancestry is replaceable")
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func hashOpenedFile(file *os.File) ([sha256.Size]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("command adapter executable: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, 0, info.Size())); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("command adapter executable: %w", err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func validateCommand(operation string, arguments []string) error {
	if err := validateToken("operation", operation); err != nil {
		return err
	}
	if len(arguments) == 0 || len(arguments) > 32 {
		return fmt.Errorf("operation %q requires 1 to 32 fixed arguments", operation)
	}
	for _, argument := range arguments {
		if strings.IndexByte(argument, 0) >= 0 || len(argument) > 4096 {
			return fmt.Errorf("operation %q has an invalid fixed argument", operation)
		}
	}
	return nil
}

func validateEnvironment(environment []string) error {
	seen := make(map[string]struct{}, len(environment))
	unsafe := map[string]struct{}{
		"BASH_ENV": {}, "ENV": {}, "GCONV_PATH": {}, "GLIBC_TUNABLES": {},
		"LD_AUDIT": {}, "LD_DEBUG": {}, "LD_LIBRARY_PATH": {}, "LD_PRELOAD": {},
		"LOCPATH": {}, "NODE_OPTIONS": {}, "PERL5OPT": {}, "PYTHONHOME": {},
		"PYTHONINSPECT": {}, "PYTHONPATH": {}, "PYTHONSTARTUP": {}, "RUBYOPT": {},
	}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" || len(key) > 128 || len(entry) > 4096 ||
			strings.IndexByte(entry, 0) >= 0 ||
			key == "WORKFORCE_IDEMPOTENCY_KEY" {
			return fmt.Errorf("command adapter environment is invalid")
		}
		if _, rejected := unsafe[key]; rejected ||
			key == "PATH" && value != "/usr/bin:/bin" {
			return fmt.Errorf("command adapter environment is unsafe")
		}
		for index, character := range key {
			if character == '_' ||
				character >= 'a' && character <= 'z' ||
				character >= 'A' && character <= 'Z' ||
				index > 0 && character >= '0' && character <= '9' {
				continue
			}
			return fmt.Errorf("command adapter environment is invalid")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("command adapter environment is invalid")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func cloneCommands(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for operation, arguments := range source {
		result[operation] = append([]string(nil), arguments...)
	}
	return result
}

func externalID(provider, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(provider + "|" + idempotencyKey))
	return "external:" + hex.EncodeToString(sum[:16])
}

var _ io.Writer = (*limitedBuffer)(nil)
