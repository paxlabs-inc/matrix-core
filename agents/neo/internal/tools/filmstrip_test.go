// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"centra/executor/tool"
)

// BROWSER-FILMSTRIP req.2 at the tool seam. These drive the REAL persistence
// mapping over REAL tool.Result / tool.Content values: an image content block is
// persisted out-of-band while the model-facing summary stays a terse placeholder
// carrying neither the bytes nor the URL. No fakes — the only injected boundary
// is the media sink itself, which is a real closure that returns a real /media
// URL (the served round-trip is proven end-to-end in the server package).

func imageResult(mime, b64 string) *tool.Result {
	return &tool.Result{Content: []tool.Content{
		{Type: tool.ContentTypeText, Text: ""},
		{Type: tool.ContentTypeImage, MimeType: mime, Data: b64},
	}}
}

// TestPersistFirstImage_OutOfBandTerseResult proves the two halves of req.2:
// (1) the image bytes are persisted and a /media URL is produced out-of-band;
// (2) the model-facing summary is a terse placeholder that contains neither the
// base64 payload nor the media URL — the transcript stays clean.
func TestPersistFirstImage_OutOfBandTerseResult(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("\xff\xd8\xff\xe0 not-a-real-jpeg-but-real-bytes"))
	var gotMime, gotData string
	m := &Manager{media: func(mime, data string) (string, error) {
		gotMime, gotData = mime, data
		return "/media/20260711abcdef.jpg", nil
	}}

	res := imageResult("image/jpeg", b64)
	url := m.persistFirstImage(res)

	if url != "/media/20260711abcdef.jpg" {
		t.Fatalf("persistFirstImage url = %q, want the minted /media URL", url)
	}
	if gotMime != "image/jpeg" || gotData != b64 {
		t.Fatalf("sink received (mime=%q, data.len=%d), want the real image block passed through", gotMime, len(gotData))
	}

	// The model-facing summary must be terse and leak neither bytes nor URL.
	summary := summarizeNonText(res)
	if strings.Contains(summary, b64) {
		t.Error("model-facing summary leaked the base64 image bytes")
	}
	if strings.Contains(summary, url) || strings.Contains(summary, "/media/") {
		t.Error("model-facing summary leaked the media URL")
	}
	if !strings.Contains(summary, "image/jpeg") {
		t.Errorf("summary should be a terse image placeholder, got %q", summary)
	}
}

// TestPersistFirstImage_BestEffort proves req.2 ac_3: a media write error is
// swallowed (url ""), never surfaced as a failure — persistence never fails the
// tool call. A nil sink is likewise a clean no-op.
func TestPersistFirstImage_BestEffort(t *testing.T) {
	res := imageResult("image/png", base64.StdEncoding.EncodeToString([]byte("bytes")))

	failing := &Manager{media: func(mime, data string) (string, error) {
		return "", fmt.Errorf("disk full")
	}}
	if url := failing.persistFirstImage(res); url != "" {
		t.Errorf("write error must yield empty url (best-effort), got %q", url)
	}

	nilSink := &Manager{}
	if url := nilSink.persistFirstImage(res); url != "" {
		t.Errorf("no sink wired must yield empty url, got %q", url)
	}

	// A result with no image block yields no URL and does not call the sink.
	textOnly := &tool.Result{Content: []tool.Content{{Type: tool.ContentTypeText, Text: "hi"}}}
	called := false
	m := &Manager{media: func(mime, data string) (string, error) { called = true; return "/media/x", nil }}
	if url := m.persistFirstImage(textOnly); url != "" || called {
		t.Errorf("text-only result must not persist (url=%q called=%v)", url, called)
	}
}

// TestCaptureViewport_NoSink proves CaptureViewport is a clean no-op when no
// media sink is wired (nowhere to write a still) — it never panics or dispatches.
func TestCaptureViewport_NoSink(t *testing.T) {
	m := &Manager{}
	if url := m.CaptureViewport(nil, "browser__browser_navigate"); url != "" {
		t.Errorf("CaptureViewport with no media sink must be a no-op, got %q", url)
	}
}
