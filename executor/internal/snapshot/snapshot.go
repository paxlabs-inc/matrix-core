// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package snapshot provides per-Machine state snapshot/restore against
// an S3-compatible object store (MinIO).
//
// Object layout (per matrix.kvx sess#25 S25Q6):
//
//	users/<user_id>/latest.tar.zst          // most recent snapshot (alias)
//	users/<user_id>/snapshots/<ts>.tar.zst  // historical snapshots (versioned)
//	users/<user_id>/meta.json               // (reserved; not used at v1)
//
// Tarball content: full <DataDir> tree (cortex/ + journal/ + transcripts/
// + workspace/ + .matrix/). The seeded sentinel is preserved so restores
// land in already-seeded state and never trigger a second pull.
//
// Concurrency: snapshots are produced via tar-while-running. Pebble's
// MANIFEST + WAL design makes copy-during-write recoverable on next
// open via WAL replay. Final on-shutdown push happens AFTER server
// drain so cortex is quiescent (the gold-standard consistency point).
//
// Implementation: shells out to `mc` (already baked into deploy/daemon/
// Dockerfile) for S3 transport and `tar -I zstd` for archive creation.
// mc receives credentials via the MC_HOST_<alias> env-var pattern so
// no on-disk alias config is written. This package adds zero new Go
// module dependencies.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config configures the snapshot Manager.
//
// DataDir, Endpoint, Bucket, and UserID are mandatory; without them,
// New returns ErrIncomplete. AccessKey / SecretKey may be empty when
// the endpoint is reachable anonymously (rare; mostly useful for
// integration testing against a public test bucket).
type Config struct {
	// DataDir is the root that the daemon persists state under
	// (cortex/, journal/, transcripts/, workspace/, .matrix/).
	// Tarballs capture this entire tree.
	DataDir string

	// Endpoint is the S3-compatible base URL,
	// e.g. http://[fdaa:75:8960:...]:9000 or https://box.matrix.wg:9000.
	// Must include scheme + host + port.
	Endpoint string

	// Bucket is the S3 bucket name (e.g. matrix-state).
	Bucket string

	// AccessKey / SecretKey are the S3 credentials. May be empty for
	// anonymous endpoints. They are passed to mc via MC_HOST_<alias>
	// env-var, never written to disk.
	AccessKey string
	SecretKey string

	// UserID is the namespace prefix under users/<UserID>/. Typically
	// the Supabase user id; falls back to the cortex actor name when
	// no Supabase identity is bound.
	UserID string

	// PushInterval controls the periodic-push ticker; zero defaults to
	// DefaultPushInterval (5 minutes). Negative disables the ticker
	// (only boot pull and shutdown push remain).
	PushInterval time.Duration

	// Logf is called with (event, fields) on every notable lifecycle
	// edge. Fields is non-nil and may be appended to. Errors include
	// an "error" key. nil disables logging.
	Logf func(event string, fields map[string]interface{})

	// Now is injectable wall clock for tests; nil defaults to time.Now.
	Now func() time.Time
}

// DefaultPushInterval is the gap between periodic snapshot pushes when
// Config.PushInterval is zero.
const DefaultPushInterval = 5 * time.Minute

// SeededSentinel is the path (relative to DataDir) that marks a Volume
// as already-seeded. New Machines pull latest.tar.zst the first time
// this file is missing; once it's present, subsequent boots skip the
// pull. The sentinel itself is part of the tarball, so a restore from
// a previous snapshot lands in seeded state.
const SeededSentinel = ".matrix/seeded"

// mcAlias is the in-process alias name used for MC_HOST_<alias>; chosen
// to avoid collision with anything an operator might define.
const mcAlias = "matrixsnap"

// ErrIncomplete is returned by New when required Config fields are
// missing. The error wraps a list of the missing fields.
var ErrIncomplete = errors.New("snapshot: config incomplete")

// ErrNoSnapshot is returned by Pull when no latest.tar.zst exists yet
// for the user (fresh-Machine, first-boot scenario).
var ErrNoSnapshot = errors.New("snapshot: no prior snapshot for user")

// Manager owns the snapshot lifecycle for one daemon process.
//
// Use New to construct, BootPull at boot, Start to launch the periodic
// ticker, Push for ad-hoc pushes, and Stop to halt the ticker and run
// a final push. Manager methods are safe to call concurrently; the
// underlying mc subprocess is serialised via pushMu.
type Manager struct {
	cfg Config

	// pushMu serialises Push calls so we never run two `mc cp` invocations
	// against the same destination at once. Pull does not contend.
	pushMu sync.Mutex

	// stopCh signals the ticker goroutine to exit.
	stopCh chan struct{}
	// doneCh is closed once the ticker goroutine has fully exited.
	doneCh chan struct{}
	// startOnce guards Start so multiple invocations don't fork
	// duplicate tickers.
	startOnce sync.Once
	// stopOnce guards Stop similarly.
	stopOnce sync.Once

	// mcEnv is the MC_HOST_<alias> value passed to every mc subprocess.
	// Computed once in New.
	mcEnv string
}

// New validates cfg and returns a Manager.
//
// Returns ErrIncomplete (wrapped with the missing-field list) when any
// required field is empty. Endpoint is parsed with url.Parse; a
// malformed URL also returns an error.
func New(cfg Config) (*Manager, error) {
	var missing []string
	if cfg.DataDir == "" {
		missing = append(missing, "DataDir")
	}
	if cfg.Endpoint == "" {
		missing = append(missing, "Endpoint")
	}
	if cfg.Bucket == "" {
		missing = append(missing, "Bucket")
	}
	if cfg.UserID == "" {
		missing = append(missing, "UserID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrIncomplete, strings.Join(missing, ","))
	}

	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("snapshot: parse endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("snapshot: endpoint missing scheme or host: %q", cfg.Endpoint)
	}

	// Build MC_HOST_<alias>=scheme://key:secret@host
	// Empty creds are rendered as "@host" which mc accepts for
	// anonymous endpoints.
	credPart := ""
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		credPart = url.QueryEscape(cfg.AccessKey) + ":" + url.QueryEscape(cfg.SecretKey) + "@"
	}
	mcEnv := fmt.Sprintf("%s://%s%s", u.Scheme, credPart, u.Host)
	if u.Path != "" && u.Path != "/" {
		mcEnv += u.Path
	}

	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}

	return &Manager{
		cfg:    cfg,
		mcEnv:  mcEnv,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}, nil
}

// remotePath returns the mc-style path "<alias>/<bucket>/<key>".
func (m *Manager) remotePath(key string) string {
	return mcAlias + "/" + m.cfg.Bucket + "/" + key
}

// userPrefix returns "users/<UserID>".
func (m *Manager) userPrefix() string {
	return "users/" + m.cfg.UserID
}

// log emits a lifecycle event when a Logf is wired; otherwise no-op.
// fields is allocated lazily when nil.
func (m *Manager) log(event string, fields map[string]interface{}) {
	if m.cfg.Logf == nil {
		return
	}
	if fields == nil {
		fields = map[string]interface{}{}
	}
	m.cfg.Logf(event, fields)
}

// runMC invokes the mc binary with the configured MC_HOST env. Returns
// (stdout, stderr, error). Errors carry the combined output for log
// inspection.
func (m *Manager) runMC(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "mc", args...)
	cmd.Env = append(os.Environ(),
		"MC_HOST_"+mcAlias+"="+m.mcEnv,
		// Suppress mc's update-check + update-prompt noise.
		"MC_DISABLE_PAGER=1",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// SeededPath is the absolute path to the sentinel file.
func (m *Manager) SeededPath() string {
	return filepath.Join(m.cfg.DataDir, SeededSentinel)
}

// IsSeeded returns true iff the sentinel file is present on disk.
func (m *Manager) IsSeeded() bool {
	_, err := os.Stat(m.SeededPath())
	return err == nil
}

// markSeeded creates the sentinel file (and parent .matrix/ dir) so
// future boots skip the pull.
func (m *Manager) markSeeded() error {
	dir := filepath.Dir(m.SeededPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir .matrix: %w", err)
	}
	f, err := os.OpenFile(m.SeededPath(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("touch seeded: %w", err)
	}
	return f.Close()
}

// BootPull restores the user's most recent snapshot into DataDir if and
// only if the sentinel file is missing. It is the caller's
// responsibility to invoke BootPull before opening the cortex Pebble DB.
//
// Behaviour matrix:
//
//   - sentinel present                 → no-op, returns (false, nil)
//   - no remote snapshot for the user  → tarball mkdir + sentinel write,
//     returns (false, ErrNoSnapshot wrapped) so caller can log fresh-start
//   - remote snapshot present          → mc cp + tar extract + sentinel
//     write, returns (true, nil)
//
// Any error from mc / tar / fs is returned without writing the sentinel,
// so a transient pull failure retries on next boot.
func (m *Manager) BootPull(ctx context.Context) (bool, error) {
	if m.IsSeeded() {
		m.log("snapshot.boot.skip", map[string]interface{}{"reason": "already_seeded"})
		return false, nil
	}
	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return false, fmt.Errorf("snapshot: mkdir data-dir: %w", err)
	}

	latestKey := m.userPrefix() + "/latest.tar.zst"
	tmp, err := os.CreateTemp("", "matrix-snapshot-pull-*.tar.zst")
	if err != nil {
		return false, fmt.Errorf("snapshot: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	m.log("snapshot.pull.start", map[string]interface{}{
		"key": latestKey,
	})
	_, stderr, err := m.runMC(ctx, "cp", "--quiet", m.remotePath(latestKey), tmpPath)
	if err != nil {
		// mc returns non-zero for "object not found" on cp; differentiate
		// by stderr inspection so we don't leak that as a hard error.
		if strings.Contains(stderr, "Object does not exist") || strings.Contains(stderr, "does not exist") || strings.Contains(stderr, "NoSuchKey") {
			if mErr := m.markSeeded(); mErr != nil {
				return false, mErr
			}
			m.log("snapshot.pull.fresh", map[string]interface{}{
				"reason": "no_prior_snapshot",
			})
			return false, ErrNoSnapshot
		}
		return false, fmt.Errorf("snapshot: mc cp pull: %w (stderr=%q)", err, stderr)
	}

	// Validate non-zero size. mc would have errored on missing object,
	// but defensive coding for partial transfers.
	st, err := os.Stat(tmpPath)
	if err != nil || st.Size() == 0 {
		return false, fmt.Errorf("snapshot: pulled empty tarball")
	}

	m.log("snapshot.pull.extract", map[string]interface{}{
		"size_bytes": st.Size(),
		"data_dir":   m.cfg.DataDir,
	})
	if err := untarZst(ctx, tmpPath, m.cfg.DataDir); err != nil {
		return false, fmt.Errorf("snapshot: untar: %w", err)
	}
	if err := m.markSeeded(); err != nil {
		return false, err
	}
	m.log("snapshot.pull.done", map[string]interface{}{
		"size_bytes": st.Size(),
	})
	return true, nil
}

// Push tar+zstd's the DataDir tree, uploads it to
// users/<uid>/snapshots/<ts>.tar.zst, then atomically updates
// users/<uid>/latest.tar.zst via a server-side copy.
//
// Returns the timestamp-suffixed object key on success. Concurrent Push
// calls serialise on pushMu (cortex-quiescence isn't promised; this
// only prevents two concurrent mc cp uploads racing the alias write).
func (m *Manager) Push(ctx context.Context) (string, error) {
	m.pushMu.Lock()
	defer m.pushMu.Unlock()

	ts := m.cfg.Now().UTC().Format("20060102T150405Z")
	snapKey := fmt.Sprintf("%s/snapshots/%s.tar.zst", m.userPrefix(), ts)
	latestKey := m.userPrefix() + "/latest.tar.zst"

	tmp, err := os.CreateTemp("", "matrix-snapshot-push-*.tar.zst")
	if err != nil {
		return "", fmt.Errorf("snapshot: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	m.log("snapshot.push.archive", map[string]interface{}{
		"data_dir": m.cfg.DataDir,
		"tmp":      tmpPath,
	})
	if err := tarZst(ctx, m.cfg.DataDir, tmpPath); err != nil {
		return "", fmt.Errorf("snapshot: tar: %w", err)
	}
	st, err := os.Stat(tmpPath)
	if err != nil {
		return "", fmt.Errorf("snapshot: stat tarball: %w", err)
	}

	m.log("snapshot.push.upload", map[string]interface{}{
		"key":        snapKey,
		"size_bytes": st.Size(),
	})
	if _, stderr, err := m.runMC(ctx, "cp", "--quiet", tmpPath, m.remotePath(snapKey)); err != nil {
		return "", fmt.Errorf("snapshot: mc cp push: %w (stderr=%q)", err, stderr)
	}

	// Server-side copy: write latest.tar.zst from snapshots/<ts>.tar.zst.
	// mc cp <remote-src> <remote-dst> performs a server-side COPY rather
	// than re-uploading; this keeps "latest" pointer-update atomic from
	// the user's perspective.
	if _, stderr, err := m.runMC(ctx, "cp", "--quiet", m.remotePath(snapKey), m.remotePath(latestKey)); err != nil {
		return "", fmt.Errorf("snapshot: mc cp alias-update: %w (stderr=%q)", err, stderr)
	}

	m.log("snapshot.push.done", map[string]interface{}{
		"key":        snapKey,
		"size_bytes": st.Size(),
	})
	return snapKey, nil
}

// Start launches the periodic-push ticker if cfg.PushInterval >= 0.
// Idempotent: subsequent calls are no-ops. Returns immediately.
//
// PushInterval == 0 → DefaultPushInterval (5 minutes).
// PushInterval  < 0 → no ticker; periodic pushes disabled (only
// BootPull and Stop's final push remain).
func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		interval := m.cfg.PushInterval
		if interval == 0 {
			interval = DefaultPushInterval
		}
		if interval < 0 {
			close(m.doneCh)
			m.log("snapshot.ticker.disabled", nil)
			return
		}
		go m.tick(ctx, interval)
		m.log("snapshot.ticker.start", map[string]interface{}{
			"interval_sec": int(interval / time.Second),
		})
	})
}

// tick runs until ctx is cancelled OR Stop closes stopCh, then closes
// doneCh and exits. Each tick fires Push with a fresh per-tick context
// derived from the parent so a long-running upload that misses the next
// tick doesn't block the ticker (each Push contends pushMu — slow
// uploads delay the next attempt rather than overlapping).
func (m *Manager) tick(parent context.Context, interval time.Duration) {
	defer close(m.doneCh)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-m.stopCh:
			return
		case <-t.C:
			pushCtx, cancel := context.WithTimeout(parent, 10*time.Minute)
			if _, err := m.Push(pushCtx); err != nil {
				m.log("snapshot.tick.error", map[string]interface{}{
					"error": err.Error(),
				})
			}
			cancel()
		}
	}
}

// Stop halts the periodic ticker and runs one final Push using ctx.
// Idempotent: subsequent calls are no-ops.
//
// Returns the error from the final Push (or nil); the ticker shutdown
// itself never errors.
func (m *Manager) Stop(ctx context.Context) error {
	var pushErr error
	m.stopOnce.Do(func() {
		close(m.stopCh)
		<-m.doneCh
		m.log("snapshot.ticker.stopped", nil)
		_, pushErr = m.Push(ctx)
		if pushErr != nil {
			m.log("snapshot.shutdown.push.error", map[string]interface{}{
				"error": pushErr.Error(),
			})
		}
	})
	return pushErr
}

// tarZst archives src/ as zstd-compressed tar at dst. Shells to
// `tar -I zstd -cf <dst> -C <src> .` so we avoid pulling a Go zstd
// dependency. The image's apt-installed zstd + tar are required at
// runtime (verified in deploy/daemon/Dockerfile).
func tarZst(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "tar", "-I", "zstd", "-cf", dst, "-C", src, ".")
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar -I zstd -cf %s -C %s .: %w (stderr=%q)", dst, src, err, stderr.String())
	}
	return nil
}

// untarZst extracts src (zstd-compressed tar) into dst/. dst must
// already exist; tar will create subdirs as needed. Shells to
// `tar -I zstd -xf <src> -C <dst>`.
func untarZst(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "tar", "-I", "zstd", "-xf", src, "-C", dst)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar -I zstd -xf %s -C %s: %w (stderr=%q)", src, dst, err, stderr.String())
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
