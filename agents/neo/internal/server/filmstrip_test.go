// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// BROWSER-FILMSTRIP at the server seam — all over the REAL Engine + Server +
// media plane + trace store (no fakes):
//
//   - req.2: a real image round-trips to a served /media/<id> asset.
//   - req.4: a browser tool.step carrying only the /media URL (never bytes)
//     survives reopen via the durable trace, and the referenced asset still
//     serves.
//   - req.7.2: the GC sweep removes stills past the retention window and leaves
//     fresh + retention-disabled media untouched.

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"centra/agents/neo/internal/agent"
	"centra/agents/neo/internal/config"
)

// realJPEG encodes a genuine (small) JPEG so the round-trip carries real image
// bytes, not a placeholder string.
func realJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for y := 0; y < 12; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 20), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func mediaServeEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	return &Engine{
		cfg:      config.Default(),
		mediaDir: dir,
		broker:   newBroker(),
		runs:     map[string]*run{},
	}
}

// TestPersistToolImage_ServedRoundTrip proves req.2 ac_1/ac_4: a real image
// content block's bytes are persisted under a minted id and served byte-for-byte
// by GET /media/<id>.
func TestPersistToolImage_ServedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	e := mediaServeEngine(t, dir)
	srv := newLiveRunTestServer(t, e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	raw := realJPEG(t)
	b64 := base64.StdEncoding.EncodeToString(raw)

	url, err := e.persistToolImage("image/jpeg", b64)
	if err != nil {
		t.Fatalf("persistToolImage: %v", err)
	}
	if !bytes.HasPrefix([]byte(url), []byte("/media/")) || filepath.Ext(url) != ".jpg" {
		t.Fatalf("url = %q, want /media/<id>.jpg", url)
	}

	resp, err := http.Get(ts.URL + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", url, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, raw) {
		t.Errorf("served bytes (%d) differ from persisted image (%d)", len(got), len(raw))
	}
}

// TestBrowserStep_ScreenshotURLSurvivesReopen proves req.4: a browser
// navigation's tool.step carries screenshot_url (and only the URL, never inline
// bytes), the trace persists it, GET /conversations/{id}/trace replays it on
// reopen, and the referenced /media asset still serves.
func TestBrowserStep_ScreenshotURLSurvivesReopen(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		MediaDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	if !e.trace.Enabled() {
		t.Fatal("trace store must be enabled")
	}
	srv := newLiveRunTestServer(t, e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// A real captured still lands on the media plane out-of-band.
	raw := realJPEG(t)
	shotURL, err := e.persistToolImage("image/jpeg", base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("persistToolImage: %v", err)
	}

	const convID = "conv_film"
	const intentID = "neo_film_run"
	r := &run{id: intentID, convID: convID}
	e.registerRun(r)
	e.broker.ensure(intentID)
	e.conv.AppendUser(convID, intentID, "browse example.com")

	// The end-of-call browser event as the agent emits it: the screenshot URL
	// rides the ToolEvent out-of-band; the model-facing Result is the terse page
	// report (never the bytes).
	e.surfaceTool(r, agent.ToolEvent{
		ID:            "b1",
		Name:          "browser__browser_navigate",
		Args:          map[string]interface{}{"url": "https://example.com"},
		Result:        "- Page URL: https://example.com\n- Page Title: Example Domain\n",
		Phase:         agent.ToolEnd,
		ScreenshotURL: shotURL,
	})
	e.trace.Flush()

	e.conv.AppendAssistant(convID, intentID, "Here is example.com.")
	e.broker.closeRun(intentID)
	e.unregisterRun(intentID)

	body := mustGetJSON(t, &http.Client{Timeout: 5 * time.Second}, ts.URL+"/conversations/"+convID+"/trace")
	rawEvents, ok := body["events"].([]interface{})
	if !ok {
		t.Fatalf("events must be a list, got %T", body["events"])
	}
	var found bool
	for _, re := range rawEvents {
		ev := re.(map[string]interface{})
		if ev["type"] != "tool.step" {
			continue
		}
		f, _ := ev["fields"].(map[string]interface{})
		if f == nil || f["surface"] != "browser" {
			continue
		}
		found = true
		if f["screenshot_url"] != shotURL {
			t.Errorf("replayed browser step screenshot_url = %v, want %q", f["screenshot_url"], shotURL)
		}
		if f["url"] != "https://example.com" {
			t.Errorf("replayed browser step url = %v", f["url"])
		}
		if f["page_title"] != "Example Domain" {
			t.Errorf("replayed browser step page_title = %v", f["page_title"])
		}
	}
	if !found {
		t.Fatal("no browser tool.step replayed from the durable trace")
	}

	// The referenced /media asset still serves the persisted bytes.
	resp, err := http.Get(ts.URL + shotURL)
	if err != nil {
		t.Fatalf("GET %s: %v", shotURL, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, raw) {
		t.Errorf("replayed screenshot asset did not serve intact (status=%d, bytes=%d/%d)", resp.StatusCode, len(got), len(raw))
	}
}

// TestSweepMedia_RemovesStalePastRetention proves req.7.2: the GC sweep deletes
// media older than the retention window and keeps fresh media; retention 0
// disables the sweep entirely.
func TestSweepMedia_RemovesStalePastRetention(t *testing.T) {
	dir := t.TempDir()
	raw := realJPEG(t)

	stale := filepath.Join(dir, "20260101stale.jpg")
	fresh := filepath.Join(dir, "20260711fresh.jpg")
	if err := os.WriteFile(stale, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Age the stale file well past a 24h retention window.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	e := &Engine{mediaDir: dir, cfg: config.Config{MediaRetentionHours: 24}}
	e.sweepMedia()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale still past retention should be swept, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh still within retention must be kept, stat err = %v", err)
	}

	// Retention disabled (0): even a stale file is untouched.
	dir2 := t.TempDir()
	stale2 := filepath.Join(dir2, "20260101stale.jpg")
	if err := os.WriteFile(stale2, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale2, old, old); err != nil {
		t.Fatal(err)
	}
	e2 := &Engine{mediaDir: dir2, cfg: config.Config{MediaRetentionHours: 0}}
	e2.sweepMedia()
	if _, err := os.Stat(stale2); err != nil {
		t.Errorf("retention disabled must not sweep, stat err = %v", err)
	}
}

// TestExtForMIME pins the served-extension mapping (viewport JPEG default).
func TestExtForMIME(t *testing.T) {
	cases := map[string]string{
		"image/jpeg": ".jpg", "image/jpg": ".jpg", "image/png": ".png",
		"image/webp": ".webp", "image/gif": ".gif", "": ".jpg", "application/x-weird": ".jpg",
	}
	for in, want := range cases {
		if got := extForMIME(in); got != want {
			t.Errorf("extForMIME(%q) = %q, want %q", in, got, want)
		}
	}
}
