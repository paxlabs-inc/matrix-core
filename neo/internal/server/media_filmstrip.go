// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BROWSER-FILMSTRIP media seam. persistToolImage is the sink wired into the tool
// manager (SetMediaPersist): it writes a tool-produced image (a browser
// screenshot) to the served /data/media plane and returns its /media/<id> URL,
// which travels out-of-band to the surfacing layer so the bytes never enter the
// model transcript (req.2). sweepMedia bounds the volume's media growth (req.7.2).

// mediaGCInterval is the periodic cadence of the retention sweep. Coarse: the
// boot sweep does the bulk and stills accumulate slowly, so an hourly-ish tick
// is ample and keeps the process outbound-quiet enough for Serverless sleep.
const mediaGCInterval = 6 * time.Hour

// persistToolImage decodes a base64 image payload and writes it to the media dir
// under a minted content id, returning its served /media URL. It mirrors the
// upload path (mintMediaID + write on the machine volume) so a screenshot is
// served by the same GET /media route as generated/uploaded media. Errors are
// returned to the caller, which treats persistence as best-effort (req.2 ac_3):
// a failure leaves the model-facing placeholder in place and never fails the
// tool call or the turn.
func (e *Engine) persistToolImage(mimeType, b64 string) (string, error) {
	dir := e.mediaDir
	if dir == "" {
		return "", fmt.Errorf("media storage is not configured")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("decode image bytes: %w", err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty image payload")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("prepare media storage: %w", err)
	}
	name := mintMediaID() + extForMIME(mimeType)
	if e.mediaSealEnabled() {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return "", fmt.Errorf("write media: %w", err)
		}
		if err := e.sealMediaBytes(name, data, f); err != nil {
			f.Close()
			_ = os.Remove(filepath.Join(dir, name))
			return "", fmt.Errorf("seal media: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("write media: %w", err)
		}
	} else if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return "", fmt.Errorf("write media: %w", err)
	}
	return "/media/" + name, nil
}

// extForMIME maps an image MIME type to a served file extension. Screenshots are
// viewport JPEG by default (req.7.1), so an unknown/blank type falls back to
// .jpg rather than an opaque .bin the GET /media route couldn't type.
func extForMIME(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	if exts, _ := mime.ExtensionsByType(m); len(exts) > 0 {
		return exts[0]
	}
	return ".jpg"
}

// startMediaGC bounds media growth on the volume (req.7.2): a boot sweep plus a
// periodic ticker delete media files older than the retention window (parallel
// to the trace retain cap). A no-op when no media dir is configured or retention
// is disabled (0), keeping the process byte-identical to the pre-feature image.
func (e *Engine) startMediaGC() {
	if e.mediaDir == "" || e.cfg.MediaRetentionHours <= 0 {
		return
	}
	e.sweepMedia()
	go func() {
		t := time.NewTicker(mediaGCInterval)
		defer t.Stop()
		for range t.C {
			e.sweepMedia()
		}
	}()
}

// sweepMedia removes served media files whose mtime is older than the retention
// window. Best-effort per file: a stat/remove error on one file never aborts the
// sweep. Directories are skipped (the media dir is flat).
func (e *Engine) sweepMedia() {
	dir := e.mediaDir
	if dir == "" || e.cfg.MediaRetentionHours <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(e.cfg.MediaRetentionHours) * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
}
