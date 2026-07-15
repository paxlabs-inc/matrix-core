// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

// vault.go — snapshot tarballs are sealed with chunked streaming AEAD before
// they leave the machine for S3/MinIO, and decrypt-verified on restore.
//
// Recovery bootstrap: the wrapped per-user keyfile (vault.key) lives INSIDE
// the data tree the tarball archives, so a fresh machine restoring a sealed
// snapshot cannot read the key it needs out of the object it is trying to
// open. Push therefore mirrors the keyfile (already wrapped under the
// platform KEK — safe at rest anywhere) as a plaintext sidecar object at
// users/<uid>/vault.key. BootPull of a sealed tarball pulls that sidecar to
// vault.KeyfilePath(DataDir) when the volume has no keyfile yet, boots a
// session from it (KEK from the environment), and decrypt-verifies the
// stream. Truncation and chunk reordering fail closed via the per-chunk
// index+final associated data of vault/stream.

import (
	"context"
	"fmt"
	"io"
	"os"

	"matrix/vault"
)

const (
	snapStore   = "executor.snapshot"
	snapSchema1 = "tar.v1"
)

// snapAD is the tarball's associated data. It deliberately binds the user
// only — the same object bytes live under snapshots/<ts>.tar.zst AND the
// server-side-copied latest.tar.zst alias, so the object key cannot be
// bound. Within the stream, chunk order and termination are bound per chunk.
func snapAD(user string) vault.AD {
	return vault.AD{User: user, Store: snapStore, Schema: snapSchema1}
}

// SetVault wires the fail-closed data-at-rest session and owning user DID.
// Called once after the daemon's vault boots and before Start; a nil session
// keeps legacy plaintext tarballs (dev/CLI).
func (m *Manager) SetVault(sess *vault.Session, user string) {
	if m == nil {
		return
	}
	m.vault = sess
	m.sealUser = user
}

// sealFile streams src into dst as chunked AEAD ciphertext. Caller has
// verified the session is encrypting.
func (m *Manager) sealFile(src, dst string) error {
	uv := m.vault.UserVault()
	if uv == nil {
		return fmt.Errorf("snapshot: seal without an encrypting vault session")
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("snapshot: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot: create %s: %w", dst, err)
	}
	sw, err := uv.StreamWriter(out, snapAD(m.sealUser), 0)
	if err != nil {
		out.Close()
		return fmt.Errorf("snapshot: stream writer: %w", err)
	}
	if _, err := io.Copy(sw, in); err != nil {
		out.Close()
		return fmt.Errorf("snapshot: seal copy: %w", err)
	}
	if err := sw.Close(); err != nil {
		out.Close()
		return fmt.Errorf("snapshot: seal close: %w", err)
	}
	return out.Close()
}

// isVaultFile sniffs whether the file at path begins with the vault magic.
func isVaultFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, 16)
	n, err := io.ReadFull(f, head)
	if err != nil && n == 0 {
		return false, err
	}
	return vault.IsVault(head[:n]), nil
}

// keyfileKey is the sidecar object carrying the wrapped per-user keyfile.
func (m *Manager) keyfileKey() string { return m.userPrefix() + "/vault.key" }

// pushKeyfile mirrors the wrapped keyfile beside the snapshots so a fresh
// machine can bootstrap decryption of a sealed tarball.
func (m *Manager) pushKeyfile(ctx context.Context) error {
	path := vault.KeyfilePath(m.cfg.DataDir)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("snapshot: keyfile %s: %w", path, err)
	}
	if _, stderr, err := m.runMC(ctx, "cp", "--quiet", path, m.remotePath(m.keyfileKey())); err != nil {
		return fmt.Errorf("snapshot: mc cp keyfile: %w (stderr=%q)", err, stderr)
	}
	return nil
}

// ensureKeyfile makes sure vault.KeyfilePath(DataDir) exists, pulling the
// sidecar object on a fresh volume. A sealed snapshot with no local keyfile
// and no sidecar is unrecoverable and errors loudly.
func (m *Manager) ensureKeyfile(ctx context.Context) error {
	path := vault.KeyfilePath(m.cfg.DataDir)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp := path + ".pull"
	if _, stderr, err := m.runMC(ctx, "cp", "--quiet", m.remotePath(m.keyfileKey()), tmp); err != nil {
		return fmt.Errorf("snapshot: sealed snapshot but keyfile sidecar unavailable: %w (stderr=%q)", err, stderr)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// openSealedTarball decrypt-verifies the sealed tarball at src into dst. The
// session is bootstrapped from the local keyfile (pulled by ensureKeyfile on
// fresh volumes): the keyfile names its owning user, and the KEK comes from
// the environment — nothing inside the sealed object is needed to open it.
func (m *Manager) openSealedTarball(ctx context.Context, src, dst string) error {
	kb, err := os.ReadFile(vault.KeyfilePath(m.cfg.DataDir))
	if err != nil {
		return fmt.Errorf("snapshot: read keyfile: %w", err)
	}
	kf, err := vault.ParseKeyfile(kb)
	if err != nil {
		return fmt.Errorf("snapshot: parse keyfile: %w", err)
	}
	sess, err := vault.Boot(ctx, vault.ConfigFromEnv(m.cfg.DataDir, kf.User))
	if err != nil {
		return fmt.Errorf("snapshot: vault boot for sealed restore: %w", err)
	}
	uv := sess.UserVault()
	if uv == nil {
		return fmt.Errorf("snapshot: sealed snapshot but no usable vault key (set VAULT_KEK / VAULT_KEK_FILE)")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	sr, err := uv.StreamReader(in, snapAD(kf.User))
	if err != nil {
		return fmt.Errorf("snapshot: stream reader: %w", err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, sr); err != nil {
		out.Close()
		return fmt.Errorf("snapshot: decrypt-verify: %w", err)
	}
	return out.Close()
}
