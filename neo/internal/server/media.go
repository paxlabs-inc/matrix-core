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

// Neo's media plane. Generated and uploaded media (images / video / audio)
// live on the agent's OWN machine volume — the same /data volume that holds
// cortex + executor.key — under MediaDir. The media MCP server (tools/media)
// writes generated outputs there; users upload inputs via POST /upload; and
// GET /media/<name> streams either back to the browser. Nothing leaves the
// per-user machine: input images are inlined to the upstream API as base64
// data URIs by the bridge, never as public URLs.

// uploadMaxBytes caps a single uploaded file (inputs to edit/animate/
// transcribe). Generous for audio + short clips; the multipart reader rejects
// anything larger.
const uploadMaxBytes = 100 << 20 // 100 MiB

// allowedUploadKinds gates uploads to media the tools can actually consume.
var allowedUploadKinds = map[string]bool{"image": true, "audio": true, "video": true}

// mediaExtMIME backstops mime.TypeByExtension for the formats the tools accept
// (the stdlib table is sparse on some platforms / for newer audio types).
var mediaExtMIME = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".m4a": "audio/mp4",
	".flac": "audio/flac", ".ogg": "audio/ogg", ".opus": "audio/opus", ".aac": "audio/aac",
}

// MediaDir resolves the directory Neo serves media from / writes uploads to.
// An explicit override wins; otherwise it derives from the cortex root's
// parent (the machine volume), matching how conversation.Dir derives
// /data/conversations — so generated media lands on /data/media and survives
// reload / suspend / redeploy.
func MediaDir(override, cortexRoot string) string {
	if o := strings.TrimSpace(override); o != "" {
		return o
	}
	if c := strings.TrimSpace(cortexRoot); c != "" {
		return filepath.Join(filepath.Dir(c), "media")
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

// safeMediaName validates a single-segment media filename (no path traversal,
// no nested directories) and returns it, or "" if invalid.
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
	// Map a few common content types the stdlib may miss.
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

// handleMedia streams a stored media file (GET /media/<name>). It is a Neo-owned
// route registered before the catch-all daemon proxy.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	dir := s.engine.mediaDir
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
// a "file" field) on the machine volume and returns its /media reference. The
// client embeds that reference in the next chat message so the model can pass
// it to edit_image / generate_video / transcribe_audio.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	dir := s.engine.mediaDir
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
