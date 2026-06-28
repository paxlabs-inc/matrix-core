# KVX Spec Preview

A VS Code extension that renders Matrix `.kvx` spec files as an interactive,
human-friendly view — the way the built-in Markdown preview renders Mermaid.
Open a `spec.kvx` or `workflow.kvx` and get a live picture of the work instead
of a wall of key/value lines.

## What it shows

**`spec.kvx` → two views (tabbed):**

- **Task map** — leaf tasks laid out in columns by dependency *wave*
  (parallelizable within a wave), colored by status (done / in progress /
  pending). Click a task to see its implementation bullets and the acceptance
  criteria it validates, and to jump to that section in the source. Hover a card
  to highlight the rest of its feature group.
- **Traceability** — the "zero code drift" view. Every requirement's acceptance
  criteria, each marked covered (a task references it) or a **gap** (no task
  implements it). Requirements with gaps auto-expand. Dangling references
  (a task pointing at a criterion that doesn't exist) are flagged separately.
  Criterion → task links are clickable and reveal the task in the editor.

**`workflow.kvx` → workflow view:** the session loop as a numbered flow with its
loop-back arrow, plus principles, cortex operations, hard rules, and the list of
generated IDE targets.

Any other `.kvx` file falls back to a clean sectioned key/value view.

Everything updates **live** as you edit the file (150 ms debounce), and clicking
nodes **reveals the matching section** in the text editor.

## Run it from source

```bash
npm install
npm run build        # or: npm run watch   (rebuilds on change)
```

Then in VS Code: open this folder and press **F5** ("Run Extension"). In the
Extension Development Host window that launches, open a `.kvx` file and run
**KVX: Open Preview to the Side** (also the preview icon in the editor title bar,
or `Cmd/Ctrl+K V`).

To produce an installable `.vsix`:

```bash
npx @vscode/vsce package
```

## Architecture

```
src/
  kvx/
    parser.ts   TS port of specgen's kvx.go grammar (order-preserving,
                + records each section's source line for reveal-in-editor)
    model.ts    Doc -> SpecModel / WorkflowModel; computes criteria coverage
    types.ts    shared model + message types (imported by both bundles)
  extension.ts  host: command, panel lifecycle, live re-parse, reveal, webview HTML/CSS
  webview/
    main.ts     webview entry: message wiring
    render.ts   all three views as plain DOM (theme-aware, no graph library)
```

The **host** owns parsing: on every edit it re-parses the document into a plain
JSON model and posts it to the webview, which only renders. esbuild produces two
bundles — `dist/extension.js` (Node/CJS, `vscode` external) and `dist/webview.js`
(browser IIFE).

Parsing mirrors `kvx.go` exactly (double-quoted scalars, bracketed lists, `#`
comments outside quotes, `${ENV}` interpolation), so the preview reflects what
`specgen` would render. Coverage is computed at acceptance-criterion granularity:
a task's `reqs = ["1.1", ...]` and a property test's `validates = "Requirements
1.1, 16.3"` (refs mined from the prose) both count toward covering `req.1 ac_1`.

### Design notes / honest limits

- The task map lays out by **wave**, not by a true dependency DAG, because tasks
  currently encode dependencies only as `wave` integers — there are no explicit
  `requires` edges yet. The parser already reads a `requires` list if present, so
  when those edges land, swapping the wave columns for a dagre/elkjs DAG layout is
  a localized change in `render.ts`.
- The webview chrome follows the project's own UI house rules (separation by
  background contrast, no glow, `#004CED` accent, Inter / JetBrains Mono) while
  adapting to the active VS Code theme via `--vscode-*` variables.
- `.md` output isn't rendered yet. Since the markdown is generated from the same
  `.kvx`, this preview already shows that information at the source. The natural
  next step for the `.md` side is a small markdown-it plugin that renders the
  embedded ```json waves``` block in `tasks.md` as the same graph.
```
