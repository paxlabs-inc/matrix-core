# Design — BROWSER-FILMSTRIP: Neo's Computer shows a live-feeling browsing screen

## Mission

Neo browses the web inside a headless browser, but the client cannot show the real
site: iframe embedding dies to `X-Frame-Options` / CSP `frame-ancestors` on most real
sites, and a proxy-rewrite approach is fragile and unsafe. Instead, capture a
screenshot each time the agent navigates or acts, and stream the sequence into the
"Neo's Computer" surface as a **filmstrip that reads as a live screen** — one still per
page, crossfading as Neo moves. The user's brain fills in "I'm watching Neo browse
live." It is the concrete expression of the house design goal: the illusion of a lot
without the overwhelm of a lot.

Two invariants make this honest rather than a fake:

1. **Every frame is a faithful capture** of what Neo actually saw at that step —
   never a synthesized or stock image. It is labeled "Neo viewed · <ts>", never
   presented as a continuous live video feed it is not.
2. **The screenshot never pollutes the model's reasoning** — the bytes and the media
   URL travel to the client out-of-band; the model-facing tool result stays terse.

## The load-bearing facts (from a full code sweep)

The feature is ~70% already built across the stack; the pieces are simply not
connected. The research is grounded in file:line so next session does not re-derive it.

### What already exists

- **Screenshot capability**: `browser_take_screenshot` is live in the Playwright MCP
  tool registry (`tools/browser/playwright-tools.json:485-529`) and in Neo's allowed
  surface (`agents/neo.json:69`). Viewport by default (exactly "the section they're
  viewing"), png/jpeg, returns a base64 image content block.
- **The media plane**: local `/data/media` volume, content-addressed immutable, with
  `POST /upload` and `GET /media/<name>` serving and `mintMediaID`
  (`neo/internal/server/media.go:121-231`). No MinIO/S3 — media is a machine-volume
  plane resolved by `MediaDir()` (`media.go:45-58`), same volume as cortex.
- **`tool.media` is already durable + replayed**: it is whitelisted in
  `traceWorkspaceTypes` (`neo/internal/server/engine.go:636-651`), persisted by the
  broker post-publish tap `recordTrace` (`sse.go:55-58`, `engine.go:660-680`), written
  as one JSONL file per run (`neo/internal/trace/trace.go`), and replayed on reopen via
  `GET /conversations/{id}/trace` → `handleTrace` (`server/server.go:299-319`).
- **The client browser frame**: `NeoStep` already carries `screenshotUrl` / `url` /
  `pageTitle` / `action` / `excerpt` (`apps/client/hooks/api/useChat.ts:132-139`);
  `parseToolStep` (`useChat.ts:295-320`) already reads `screenshot_url`; and
  `BrowserView` renders a full browser chrome — traffic dots, back/forward/reload, a
  secure address bar with live favicon, a Shot/Live toggle, and a progress bar
  (`apps/client/components/matrix/neo/neo-workspace.tsx:241-366`).
- **The filmstrip scrubber**: `buildScreens` already makes each browser step its own
  screen (`neo-computer.tsx:116-222`), and `NeoComputer` shows one at a time with a
  crossfade (`AnimatePresence mode="wait"`, `:367-377`), a tab strip, and a
  follow-newest "Live" pill (`:256-266, 351-360`). `buildTaskFromTrace`
  (`useChat.ts:467-624`) rebuilds the identical workspace on reopen through the same
  reducer helpers.

### The single broken line

A screenshot's pixels die at exactly one place. `summarizeNonText` collapses an image
content block to the literal string `[image image/png]` and **throws away `c.Data`**
(`neo/internal/tools/tools.go:1010-1031`) — even though the base64 is intact one layer
up in the executor (`executor/tool/registry.go:453-461`, `executor/mcp/protocol.go:179-191`).
So the screenshot never reaches the media plane, the event stream, or the client. That
is the seam this feature closes.

## The infrastructure change: per-user Playwright on Railway

The shared remote Playwright ran on Fly (`MATRIX_BROWSER_URL=http://matrix-browser.flycast:8931/mcp`,
`tools/browser/browser.mjs:23-25`) — a thin stdio↔Streamable-HTTP proxy to one shared
`@playwright/mcp`. That topology forced per-navigation capture to be a pull over the
shared bridge one screenshot at a time, with real round-trip latency, and no CDP
screencast channel.

The Railway migration changes the calculus: each user gets a dedicated 24 vCPU / 48 GB
machine, so we **bake a per-user Playwright into the daemon image**. `MATRIX_BROWSER_URL`
points at the local in-container instance; the existing `browser.mjs` bridge forwards to
it unchanged in wire shape (`browser.mjs:107-142, 170-213`). Captures become local and
cheap, removing the shared-remote latency concern and opening the door to a future
CDP screencast (out of scope for v1, noted as a v2 path).

## Architecture — the recommended approach

Rather than invent a new event type + reducer collection + component, attach a still to
each browser `tool.step` and let the **existing** `buildScreens` turn the sequence of
navigations into the filmstrip. Smallest change, maximum reuse, and durability comes
free because `tool.step` is already whitelisted and replayed.

```
agent navigates (browser_navigate / _click / submit)
        │
        ▼
dispatch auto-fires a viewport JPEG screenshot on the same MCP session   [task 2.1]
        │
        ▼
image content block → persist bytes to MediaDir, mint /media/<id>        [task 1.2]
   (model-facing result string stays terse; URL rides ToolEvent out-of-band)
        │
        ▼
surfaceTool enriches the browser tool.step with action/url/page_title/screenshot_url  [task 2.2]
        │
        ▼  (tool.step already whitelisted → persisted → replayed on reopen)
client: parseToolStep → NeoTask.steps → buildScreens → one screen per page
        │
        ▼
BrowserView renders the still (loaded via the AUTHED media loader) inside the
existing browser chrome; tab strip + crossfade + Live pill = the filmstrip   [task 3.1]
```

### Server seam detail

- **Persist, don't discard** (`tools.go:1010-1031`): when a content block is an image,
  write the bytes to `MediaDir` (reuse the `mintMediaID` + write logic from
  `media.go:121-231`) and produce a `/media/<id>` URL. The URL is carried out-of-band on
  a new `ToolEvent` field (e.g. `ScreenshotURL string`, `agent.go:72-79`) so the
  model-facing `Result` string can stay `[screenshot]` — no bytes, no URL in the
  transcript (consistent with the config-filter / no-jargon posture).
- **Auto-capture** (`tools.go` dispatch): after a successful view-changing browser tool
  (`browser_navigate`, `browser_navigate_back`, `browser_click`, form submit), issue a
  follow-up `browser_take_screenshot` (viewport, JPEG, quality-bounded) on the same MCP
  session, run it through the persist path, and attach the URL. Deterministic and
  model-invisible (no token cost, no reliance on the model choosing to screenshot).
  Gated behind `NEO_BROWSER_AUTOSHOT` (default on) with a per-run cap.
- **Enrich the step** (`surfaceTool`, `engine.go:753-810`): populate the browser
  `tool.step` fields with `action` / `url` / `page_title` / `screenshot_url`. The
  client parser already consumes all four. No new event type, no `traceWorkspaceTypes`
  change.

### Client seam detail

- **Authed still loading** (`neo-workspace.tsx:333-341`): `BrowserView`'s screenshot
  render is a plain unauthed `<img src={step.screenshotUrl}>`, but `/media` refs are
  auth-gated. Switch to the `loadMediaObjectURL` blob pattern already used by
  `NeoMediaItem` (`apps/client/lib/api/media.ts:65-74`), revoking the object URL on
  unmount.
- **Filmstrip falls out of `buildScreens`**: each navigation, being a distinct
  `ToolEvent.ID`, becomes its own sibling screen. The one thing to verify before
  building is that browser steps do not share an id (which would make `upsertStep`
  overwrite instead of accumulate — `useChat.ts:284-292`).
- **Truthful labels + house rules**: label "Neo viewed · <ts>"; separation via
  background contrast only (no border strokes for depth), no glow, no emojis. The
  simulated device chrome inside `BrowserView` keeps its deliberate dark-zinc window
  idiom (`neo-workspace.tsx:251-252`), which is device chrome, not app-layer depth.

## Non-goals

- **No Matrix/homeserver, no iframe embedding, no proxy-rewrite of sites.** The whole
  point is to avoid the iframe/CSP problem.
- **No CDP screencast in v1.** Per-navigation pull is the model; a real screencast off
  the local per-user Playwright is a v2 path, explicitly deferred.
- **No screenshot bytes or media URL in the model transcript.** The still is a
  client-only artifact; the model sees a terse placeholder.
- **No change to the MCL signed walk or any value-transfer surface.** This is a
  pure observability surface; it signs nothing and moves nothing.

## No-fakes verification

Every test exercises real code paths — a real image content block through the real
`tools.go` dispatch, the real `MediaDir` persistence + `GET /media` serving, the real
`tool.step` → trace → `handleTrace` replay, and the real client reducer
(`parseToolStep` / `buildScreens` / `BrowserView`). The end-to-end proof drives a real
navigation sequence and asserts: (1) each nav produces a persisted, servable frame;
(2) the model transcript is free of bytes/URL; (3) after reopen the browser steps
replay with their `screenshot_url`s and the filmstrip rebuilds; (4) stills load through
the authed loader. No stub/mock/fake doubles.
