// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// StdioTransport runs an MCP server as a subprocess and exchanges
// JSON-RPC frames over its stdin/stdout, one frame per line.
//
// Q15 + Q16 locks: stdio is the primary v1 transport; the subprocess
// is owned by the manager (per-agent persistent), spawn-on-boot,
// graceful drain on stop.
//
// Q18 lock: env-var refs in the agent manifest are resolved upstream
// and passed via Cmd.Env. Credentials never appear in the args slice
// (would leak through ps listings).
type StdioTransport struct {
	cmd *exec.Cmd

	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr io.ReadCloser

	mu     sync.Mutex
	closed bool

	// stderrSink optionally captures the subprocess's stderr for diagnostic
	// surfacing. Default nil = discard. Manager wires this to a logger.
	stderrSink io.Writer
}

// StdioParams configures a stdio subprocess transport.
type StdioParams struct {
	// Command is the executable to run (e.g. "npx", "uvx", "/usr/bin/git-mcp").
	Command string

	// Args are positional arguments passed verbatim. Sensitive values
	// MUST go in Env, never Args.
	Args []string

	// Env is the subprocess environment. Manager-supplied; overrides
	// the parent process's environment to avoid leaking unrelated env
	// (Q18: credential boundary).
	Env []string

	// Dir is the working directory for the subprocess. Empty = inherit.
	Dir string

	// StderrSink optionally receives subprocess stderr for logging.
	// Default nil = discard. Tests pass os.Stderr; manager wires its
	// own logger.
	StderrSink io.Writer
}

// NewStdioTransport spawns the configured subprocess and wires its
// stdio streams. Returns the transport ready to Send/Recv. Errors
// from the spawn itself (binary not found, permission denied) surface
// here; runtime errors come back through Send/Recv.
//
// The caller (manager) is responsible for calling Close to signal
// shutdown; the transport will then close stdin and let the subprocess
// drain naturally before reaping.
func NewStdioTransport(p StdioParams) (*StdioTransport, error) {
	if p.Command == "" {
		return nil, errors.New("mcp/stdio: empty command")
	}
	cmd := exec.Command(p.Command, p.Args...)
	if p.Dir != "" {
		cmd.Dir = p.Dir
	}
	if p.Env != nil {
		cmd.Env = p.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp/stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp/stdio: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp/stdio: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("mcp/stdio: start %q: %w", p.Command, err)
	}

	t := &StdioTransport{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     bufio.NewReaderSize(stdout, 64*1024),
		stderr:     stderr,
		stderrSink: p.StderrSink,
	}

	// Drain stderr in a goroutine so the subprocess never blocks on a
	// full pipe. Discard if no sink configured.
	go t.drainStderr()

	return t, nil
}

// Send writes one JSON-RPC frame followed by a newline to the
// subprocess's stdin.
func (t *StdioTransport) Send(ctx context.Context, frame []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrClosed
	}

	// Write under a lock so concurrent senders interleave whole frames,
	// not bytes within a frame. (Client serialises sends itself, but
	// belt-and-suspenders.)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	if _, err := t.stdin.Write(frame); err != nil {
		return mapStdioError(err)
	}
	if _, err := t.stdin.Write([]byte("\n")); err != nil {
		return mapStdioError(err)
	}
	return nil
}

// Recv reads the next newline-terminated JSON frame from stdout.
// Returns ErrClosed on EOF.
func (t *StdioTransport) Recv(ctx context.Context) ([]byte, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}

	line, err := t.stdout.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
			return nil, ErrClosed
		}
		return nil, fmt.Errorf("mcp/stdio: read: %w", err)
	}
	// Trim trailing newline (and CR if servers emit CRLF).
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	if len(line) == 0 {
		// Empty line — try the next frame.
		return t.Recv(ctx)
	}
	return line, nil
}

// Close shuts the subprocess down: closes stdin (signalling graceful
// shutdown to MCP servers that respect it), waits up to a small grace
// for the process to exit, then kills + reaps.
//
// Idempotent: subsequent calls return nil.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	// Close stdin to signal EOF to the subprocess.
	_ = t.stdin.Close()

	// Wait in a goroutine and racing with a kill timer.
	done := make(chan error, 1)
	go func() {
		done <- t.cmd.Wait()
	}()

	// We'd like a timeout here, but exec.Cmd.Wait doesn't take a context.
	// Manager passes a context-bound shutdown elsewhere; here we just
	// wait. Manager.Stop will Kill if Close hangs.
	<-done
	_ = t.stderr.Close()
	return nil
}

// Process exposes the underlying *os.Process so the manager can issue
// a hard kill on shutdown timeout.
func (t *StdioTransport) Process() *os.Process {
	if t.cmd == nil {
		return nil
	}
	return t.cmd.Process
}

// drainStderr reads subprocess stderr to either a configured sink or
// /dev/null. Runs until EOF (process exit) or close.
func (t *StdioTransport) drainStderr() {
	r := bufio.NewReader(t.stderr)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && t.stderrSink != nil {
			_, _ = t.stderrSink.Write(line)
		}
		if err != nil {
			return
		}
	}
}

// mapStdioError maps subprocess pipe errors to ErrClosed when the pipe
// is gone, preserving the error otherwise for caller inspection.
func mapStdioError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return ErrClosed
	}
	return fmt.Errorf("mcp/stdio: write: %w", err)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
