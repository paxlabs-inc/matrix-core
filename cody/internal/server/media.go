// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cody's media plane mirrors Neo's exactly (neo/internal/server/media.go): the
// user's machine volume IS the storage. Uploads land under MediaDir (derived
// as <volume>/media — the SAME directory Neo uses on a shared /data volume, so
// one machine has one media plane visible to both agents) and are served back
// from GET /media/<name>. The client embeds the returned reference in the next
// chat message (e.g. a design screenshot Cody should match); nothing ever
// leaves the per-user machine.

// uploadMaxBytes caps a single uploaded file.
const uploadMaxBytes = 100 << 20 // 100 MiB

// allowedUploadKinds gates uploads to media the surface can actually render.
var allowedUploadKinds = map[string]bool{"image": true, "audio": true, "video": true}

// mediaExtMIME backstops mime.TypeByExtension for the accepted formats.
var mediaExtMIME = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".m4a": "audio/mp4",
	".flac": "audio/flac", ".ogg": "audio/ogg", ".opus": "audio/opus", ".aac": "audio/aac",
}

// MediaDir resolves the directory codyd serves media from / writes uploads to.
// An explicit override wins; otherwise it derives from the data dir's parent
// (the machine volume): /data/cody -> /data/media, matching Neo's derivation
// from the cortex root so both agents share one media plane.
func MediaDir(override, dataDir string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if d := strings.TrimSpace(dataDir); d != "" {
		return filepath.Join(filepath.Dir(d), "media")
	}
	return ""
}

// mimeForName returns the MIME type for a media filename.
func mimeForName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := mediaExtMIME[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}

// kindForMIME buckets a MIME type into the surface kind the client renders.
func kindForMIME(m string) string {
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "video/"):
		return "video"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	default:
		return "file"
	}
}

// safeMediaName validates a single-segment media filename (no path traversal).
func safeMediaName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return ""
	}
	return name
}

// extForUpload picks a safe extension from the uploaded filename, falling back
// to the declared content type. Returns "" if neither yields a known media ext.
func extForUpload(filename, contentType string) string {
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if _, ok := mediaExtMIME[ext]; ok {
			return ext
		}
	}
	if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
		return exts[0]
	}
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "audio/mpeg":
		return ".mp3"
	case "video/mp4":
		return ".mp4"
	}
	return ""
}

func mintMediaID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s%s", time.Now().UTC().Format("20060102"), hex.EncodeToString(b[:]))
}

// mediaDir resolves the live media directory for this engine.
func (e *Engine) mediaDir() string {
	return MediaDir(e.opts.MediaDir, e.opts.DataDir)
}

// handleMedia streams a stored media file (GET /media/<name>).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	dir := s.engine.mediaDir()
	if dir == "" {
		http.Error(w, "media storage is not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := safeMediaName(strings.TrimPrefix(r.URL.Path, "/media/"))
	if name == "" {
		http.Error(w, "bad media reference", http.StatusBadRequest)
		return
	}
	full := filepath.Join(dir, name)
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "media not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mimeForName(name))
	// Content is immutable (content-addressed by random id); cache hard.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleUpload stores a user-uploaded media input (POST /upload, multipart with
// a "file" field) on the machine volume and returns its /media reference.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	dir := s.engine.mediaDir()
	if dir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "media storage is not configured"})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart form required (field 'file')"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a 'file' part is required"})
		return
	}
	defer file.Close()
	if header.Size > uploadMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "file too large"})
		return
	}

	ext := extForUpload(header.Filename, header.Header.Get("Content-Type"))
	if ext == "" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported file type (images, audio, and video only)"})
		return
	}
	m := mimeForName("x" + ext)
	kind := kindForMIME(m)
	if !allowedUploadKinds[kind] {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported file type (images, audio, and video only)"})
		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot prepare media storage"})
		return
	}
	name := mintMediaID() + ext
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot store upload"})
		return
	}
	written, copyErr := io.Copy(dst, file)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(filepath.Join(dir, name))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to write upload"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"url":   "/media/" + name,
		"name":  name,
		"kind":  kind,
		"mime":  m,
		"bytes": written,
	})
}
