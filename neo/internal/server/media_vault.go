// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"matrix/vault"
)

// Media-plane data-at-rest sealing (ORACLE task 2.4, media half).
//
// The GET /media read path ALWAYS sniffs: a sealed file decrypt-streams to
// the response, a legacy plaintext file serves exactly as before — so the
// serve path is seal-ready regardless of which writer produced the file.
//
// The WRITE side (uploads + browser screenshots) seals only when the vault is
// up AND NEO_MEDIA_SEAL is explicitly truthy. Default remains plaintext
// because two out-of-process readers consume media files raw off the volume:
// the Node media bridge (tools/media resolveLocalRef -> readFile feeds
// edit/animate/transcribe inputs upstream) and agent file reads of uploaded
// documents (/data/media/<name>). Turning the flag on before those readers
// decrypt through the daemon breaks image editing and document reads — the
// flip is an owner decision tracked in the ORACLE spec, not a silent default.
const envMediaSeal = "NEO_MEDIA_SEAL"

const (
	storeMedia   = "neo.media"
	schemaMedia1 = "media.v1"
)

// mediaAD binds a media object to this user and its served name — a sealed
// file renamed or copied between users fails authentication.
func (e *Engine) mediaAD(name string) vault.AD {
	return vault.AD{User: e.vaultUser, Store: storeMedia, Stream: name, Schema: schemaMedia1}
}

// mediaSealEnabled reports whether new media writes are sealed.
func (e *Engine) mediaSealEnabled() bool {
	if !e.vault.Encrypting() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envMediaSeal))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// sealMediaBytes seals a whole in-memory media payload (screenshot persist)
// as a chunked stream so GET /media serves it without buffering.
func (e *Engine) sealMediaBytes(name string, data []byte, dst io.Writer) error {
	uv := e.vault.UserVault()
	if uv == nil {
		return fmt.Errorf("media: seal without an encrypting vault session")
	}
	sw, err := uv.StreamWriter(dst, e.mediaAD(name), 0)
	if err != nil {
		return err
	}
	if _, err := sw.Write(data); err != nil {
		return err
	}
	return sw.Close()
}

// sealMediaCopy streams an upload through the vault into dst, returning the
// PLAINTEXT byte count (the payload size the client cares about).
func sealMediaCopy(e *Engine, name string, dst io.Writer, src io.Reader) (int64, error) {
	uv := e.vault.UserVault()
	if uv == nil {
		return 0, fmt.Errorf("media: seal without an encrypting vault session")
	}
	sw, err := uv.StreamWriter(dst, e.mediaAD(name), 0)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(sw, src)
	if err != nil {
		return n, err
	}
	return n, sw.Close()
}

// sniffVaultFile reports whether the opened file begins with the vault magic,
// leaving the read offset back at the start.
func sniffVaultFile(f *os.File) (bool, error) {
	head := make([]byte, 16)
	n, err := f.ReadAt(head, 0)
	if err != nil && n == 0 {
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}
	return vault.IsVault(head[:n]), nil
}

// serveSealedMedia decrypt-streams a sealed media file to the response. No
// byte-range support: the plaintext length is not knowable without a full
// pass, so the whole object streams (screenshots and images are small; large
// sealed video trades scrubbing for confidentiality until a seekable reader
// lands).
func (e *Engine) serveSealedMedia(w http.ResponseWriter, r *http.Request, f *os.File, name string) {
	uv := e.vault.UserVault()
	if uv == nil {
		http.Error(w, "sealed media requires the data vault", http.StatusServiceUnavailable)
		return
	}
	sr, err := uv.StreamReader(f, e.mediaAD(name))
	if err != nil {
		http.Error(w, "media unreadable", http.StatusInternalServerError)
		return
	}
	m := mimeForName(name)
	w.Header().Set("Content-Type", m)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if kindForMIME(m) == "file" {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	}
	if r.Method == http.MethodHead {
		return
	}
	// An authentication failure mid-stream aborts the copy; the client sees a
	// truncated body rather than attacker-controlled plaintext.
	_, _ = io.Copy(w, sr)
}
