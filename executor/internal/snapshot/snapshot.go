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
// Implementation: S3 operations use an in-process client so endpoint, bucket,
// and credentials never appear in subprocess environments or command lines.
// Archive creation still uses tar + zstd with a scrubbed child environment.
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

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"matrix/executor/tool"
	"matrix/vault"
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
// underlying S3 operations are serialized via pushMu.
type Manager struct {
	cfg Config

	// pushMu serializes Push calls so we never run two `mc cp` invocations
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

	s3 *minio.Client

	// vault + sealUser seal the snapshot tarball before it leaves the
	// machine (vault.go). Wired via SetVault after the daemon's vault
	// boots; nil keeps legacy plaintext tarballs.
	vault    *vault.Session
	sealUser string
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

	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("snapshot: endpoint paths are not supported")
	}
	creds := credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	s3, err := minio.New(u.Host, &minio.Options{
		Creds: creds, Secure: u.Scheme == "https",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot: create S3 client: %w", err)
	}

	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}

	return &Manager{
		cfg:    cfg,
		s3:     s3,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}, nil
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
// Behavior matrix:
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
	err = m.s3.FGetObject(ctx, m.cfg.Bucket, latestKey, tmpPath, minio.GetObjectOptions{})
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.StatusCode == 404 || response.Code == "NoSuchKey" || response.Code == "NoSuchObject" {
			if mErr := m.markSeeded(); mErr != nil {
				return false, mErr
			}
			m.log("snapshot.pull.fresh", map[string]interface{}{
				"reason": "no_prior_snapshot",
			})
			return false, ErrNoSnapshot
		}
		return false, fmt.Errorf("snapshot: S3 pull: %w", err)
	}

	// Validate non-zero size. mc would have errored on missing object,
	// but defensive coding for partial transfers.
	st, err := os.Stat(tmpPath)
	if err != nil || st.Size() == 0 {
		return false, fmt.Errorf("snapshot: pulled empty tarball")
	}

	// Sniff the pulled object: a sealed snapshot is decrypt-verified through
	// the vault (keyfile sidecar bootstraps a fresh volume); legacy plaintext
	// extracts exactly as before.
	sealed, err := isVaultFile(tmpPath)
	if err != nil {
		return false, fmt.Errorf("snapshot: sniff tarball: %w", err)
	}
	tarPath := tmpPath
	if sealed {
		if err := m.ensureKeyfile(ctx); err != nil {
			return false, err
		}
		plainPath := tmpPath + ".plain"
		defer os.Remove(plainPath)
		if err := m.openSealedTarball(ctx, tmpPath, plainPath); err != nil {
			return false, err
		}
		tarPath = plainPath
	}

	m.log("snapshot.pull.extract", map[string]interface{}{
		"size_bytes": st.Size(),
		"data_dir":   m.cfg.DataDir,
		"sealed":     sealed,
	})
	if err := untarZst(ctx, tarPath, m.cfg.DataDir); err != nil {
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
// calls serialize on pushMu (cortex-quiescence isn't promised; this
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
	// Seal the tarball before it leaves the machine (vault.go). The wrapped
	// keyfile is mirrored beside it so a fresh machine can bootstrap the
	// decrypt on restore. A nil/plaintext session pushes legacy plaintext.
	uploadPath := tmpPath
	if m.vault.Encrypting() {
		encPath := tmpPath + ".enc"
		defer os.Remove(encPath)
		if err := m.sealFile(tmpPath, encPath); err != nil {
			return "", err
		}
		if err := m.pushKeyfile(ctx); err != nil {
			return "", err
		}
		uploadPath = encPath
	}
	st, err := os.Stat(uploadPath)
	if err != nil {
		return "", fmt.Errorf("snapshot: stat tarball: %w", err)
	}

	m.log("snapshot.push.upload", map[string]interface{}{
		"key":        snapKey,
		"size_bytes": st.Size(),
		"sealed":     uploadPath != tmpPath,
	})
	if _, err := m.s3.FPutObject(
		ctx, m.cfg.Bucket, snapKey, uploadPath, minio.PutObjectOptions{},
	); err != nil {
		return "", fmt.Errorf("snapshot: S3 push: %w", err)
	}

	_, err = m.s3.CopyObject(
		ctx,
		minio.CopyDestOptions{Bucket: m.cfg.Bucket, Object: latestKey},
		minio.CopySrcOptions{Bucket: m.cfg.Bucket, Object: snapKey},
	)
	if err != nil {
		return "", fmt.Errorf("snapshot: S3 alias update: %w", err)
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
// runtime (verified in deploy/railway/Dockerfile).
func tarZst(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "tar", "-I", "zstd", "-cf", dst, "-C", src, ".")
	cmd.Env = tool.AgentEnvironment(os.Environ())
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
	cmd.Env = tool.AgentEnvironment(os.Environ())
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar -I zstd -xf %s -C %s: %w (stderr=%q)", src, dst, err, stderr.String())
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
