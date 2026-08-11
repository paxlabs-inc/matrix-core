// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

// BROWSER-FILMSTRIP req.8 — the no-fakes end-to-end proof of the capture path.
// It drives the REAL browser: the real tools.Manager spawns the real
// tools/browser/browser.mjs bridge, which forwards to a real @playwright/mcp
// (per-user, local — MATRIX_BROWSER_URL), performs a REAL navigation, and the
// REAL auto-capture + persist path turns the page into a served frame while the
// model-facing content stays free of bytes/URL. No stub, no canned image.
//
// It is gated on NEO_BROWSER_E2E=1 + a reachable MATRIX_BROWSER_URL so the
// hermetic suite (go test ./...) skips it cleanly, exactly like the AGON Railway
// integration test. Run it with a live browser:
//
//	PLAYWRIGHT_BROWSERS_PATH=/root/.cache/ms-playwright \
//	  node .../@playwright/mcp/cli.js --headless --isolated --browser chromium \
//	  --no-sandbox --host 127.0.0.1 --port 8931 --allowed-hosts '*' &
//	NEO_BROWSER_E2E=1 MATRIX_BROWSER_URL=http://127.0.0.1:8931/mcp \
//	  go test ./internal/tools/ -run TestBrowserFilmstrip_EndToEnd -v

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/executor/tool"
)

func TestBrowserFilmstrip_EndToEnd(t *testing.T) {
	if os.Getenv("NEO_BROWSER_E2E") != "1" {
		t.Skip("set NEO_BROWSER_E2E=1 and MATRIX_BROWSER_URL to a live @playwright/mcp to run the browser e2e")
	}
	if strings.TrimSpace(os.Getenv("MATRIX_BROWSER_URL")) == "" {
		t.Fatal("NEO_BROWSER_E2E=1 requires MATRIX_BROWSER_URL to point at a live browser")
	}

	// Build a single-server manifest from the REAL agents/neo.json browser entry
	// (so the tool bijection matches the pinned playwright-tools.json exactly).
	manifestPath := browserOnlyManifest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := Spawn(ctx, Options{ManifestPath: manifestPath, SpawnTimeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("spawn browser manager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if len(m.warnings) > 0 {
		t.Fatalf("browser server did not spawn cleanly: %v", m.warnings)
	}

	// Real media sink: decode the screenshot and write it to a temp dir, keyed by
	// the served path, so we can assert a real, servable frame landed.
	dir := t.TempDir()
	var mu sync.Mutex
	saved := map[string][]byte{}
	m.SetMediaPersist(func(mime, b64 string) (string, error) {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return "", err
		}
		name := "shot-" + strings.ReplaceAll(mime, "/", "_") + ".bin"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return "", err
		}
		mu.Lock()
		saved["/media/"+name] = data
		mu.Unlock()
		return "/media/" + name, nil
	})

	// 1) REAL navigation. It returns the page report as text; no image block, so
	//    the dispatch shot is empty and the content is the model-facing report.
	content, shot, isErr, err := m.DispatchMedia(ctx, "browser__browser_navigate",
		map[string]interface{}{"url": "https://example.com"})
	if err != nil || isErr {
		t.Fatalf("navigate failed: err=%v isErr=%v content=%q", err, isErr, content)
	}
	if shot != "" {
		t.Errorf("navigate itself returns no image, want empty shot, got %q", shot)
	}
	if !strings.Contains(content, "example.com") {
		t.Fatalf("navigate content did not look like a page report: %q", truncE2E(content))
	}

	// 2) The deterministic auto-capture on that same session → a persisted,
	//    servable JPEG frame.
	url := m.CaptureViewport(ctx, "browser__browser_navigate")
	if url == "" {
		t.Fatal("auto-capture produced no frame from a real navigation")
	}
	mu.Lock()
	frame := saved[url]
	mu.Unlock()
	if !isJPEG(frame) {
		t.Fatalf("captured frame is not a real JPEG (len=%d, url=%q)", len(frame), url)
	}

	// 3) A direct browser_take_screenshot round-trips through DispatchMedia: the
	//    URL rides out-of-band (shot), and the model-facing content carries
	//    NEITHER the bytes NOR the URL (the transcript stays clean, req.8.1).
	content2, shot2, isErr2, err2 := m.DispatchMedia(ctx, "browser__browser_take_screenshot",
		map[string]interface{}{"type": "jpeg"})
	if err2 != nil || isErr2 {
		t.Fatalf("screenshot failed: err=%v isErr=%v", err2, isErr2)
	}
	if shot2 == "" {
		t.Fatal("direct screenshot produced no out-of-band media URL")
	}
	mu.Lock()
	frame2 := saved[shot2]
	mu.Unlock()
	if !isJPEG(frame2) {
		t.Fatalf("direct screenshot frame is not a real JPEG (len=%d)", len(frame2))
	}
	if strings.Contains(content2, shot2) || strings.Contains(content2, "/media/") {
		t.Errorf("model-facing screenshot result leaked the media URL: %q", truncE2E(content2))
	}
	if b64 := base64.StdEncoding.EncodeToString(frame2); strings.Contains(content2, b64[:min(64, len(b64))]) {
		t.Error("model-facing screenshot result leaked the image bytes")
	}
}

// browserOnlyManifest reads the real agents/neo.json, keeps only the browser
// server, and writes it to a temp manifest so Spawn's tool bijection matches the
// pinned browser tool list without pulling in fs/exec/etc.
func browserOnlyManifest(t *testing.T) string {
	t.Helper()
	src := os.Getenv("MATRIX_NEO_MANIFEST")
	if src == "" {
		src = filepath.Clean("../../../agents/neo.json")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read neo manifest %s: %v", src, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	var servers []map[string]json.RawMessage
	if err := json.Unmarshal(doc["servers"], &servers); err != nil {
		t.Fatalf("parse servers: %v", err)
	}
	var browser map[string]json.RawMessage
	for _, s := range servers {
		var alias string
		_ = json.Unmarshal(s["alias"], &alias)
		if alias == "browser" {
			browser = s
			break
		}
	}
	if browser == nil {
		t.Fatal("no browser server in neo.json")
	}
	onlyBrowser, _ := json.Marshal([]map[string]json.RawMessage{browser})
	doc["servers"] = onlyBrowser
	out, _ := json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "browser-only.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	return path
}

func isJPEG(b []byte) bool {
	return len(b) > 3 && b[0] == 0xff && b[1] == 0xd8 && b[len(b)-2] == 0xff && b[len(b)-1] == 0xd9
}

func truncE2E(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// referenced so the tool package's real Content type is exercised in this file.
var _ = tool.ContentTypeImage
