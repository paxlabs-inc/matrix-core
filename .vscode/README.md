# Matrix KVX + MatrixScript

Syntax highlighting for two Matrix-stack file formats and Paxeer-branded icons for the directories that host them:

- **`.kvx`** — dense-key-value format (MATRIX.CTX session-boot files)
- **`.mtx`** — MatrixScript, the declarative language the MCL compiler is written in

Plus a devicon-backed language icon pack covering ~77 common dev tools.

## What you get

### Languages

| Language     | Extension      | Also matches                |
| ------------ | -------------- | --------------------------- |
| Matrix KVX   | `.kvx`         | `MATRIX.CTX`, `matrix.ctx`  |
| MatrixScript | `.mtx`         | `SKILL.mtx`                 |

### MatrixScript grammar (`.mtx`)

Scopes for:
- `§SECTION` headers (`§SKILL`, `§INPUTS`, `§PROCEDURE`, `§OUTPUTS`, `§FAILURE_MODES`, `§HASH`, …)
- Typed slot declarations: `slot target: ArtifactRef`, `slot style: enum<formal|casual|technical>`, `slot constraints: Constraint[]`
- Built-in types — scalar (`string`, `uint`, `float`, `bool`, `ulid`, `iso8601`, `did`, `sha256`) and MCL domain (`ArtifactRef`, `Constraint`, `Predicate`, `Unknown`, `PlanDraft`, `SkillRef`, `ToolRef`, `CortexRef`, `SlotPath`, `AssetAmount`, `AgentRef`, `UserRef`)
- `enum<a|b|c>` type expressions and `[]` list suffixes
- Block keywords: `on`, `end`, `prompt`, `resolve`, `unknown`, `clarify` — with `prompt=…` correctly disambiguated as a kv key inside `clarify` blocks
- Condition identifiers: `verb=`, `confidence<`, `on unknown`
- Resolve arrow: `slot.target <- cortex.find(...)`
- Cortex calls: `cortex.find`, `cortex.resolve`, `cortex.context`, `cortex.bundle`
- Slot paths: `slot.target`, `slot.target.prose`
- D7 closed verb vocabulary: `find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate`
- Extension verb prefix: `x:brainstorm`
- Severity (`blocking`, `preferred`), action (`fail`, `retry`, `gate`), suggestion (`raise_budget`, `extend_deadline`, `amend_constraint`, `abandon`)
- Failure reasons: `unknown_information`, `policy_violation`, `out_of_budget`, `out_of_scope`, `ambiguous_request`, `tool_failure`, `external_failure`, `timeout`, `cancelled_by_user`, `correction_invalid`
- Determinism (`seedable`, `best_effort`) and seed policies (`per_intent`, `per_session`, `per_actor`)
- Prompt role keys: `system=`, `user=`, `assistant=`
- Modifiers on their own lines: `required`, `optional`
- `matrix://…` URIs (first-class, unquoted)
- `did:method:identifier` literals (first-class, unquoted)
- Slot interpolation inside strings: `{prose}`, `{verb}`, `{slots}`, `{unknowns}`, `{cortex.bundle}`, `{slot.target}`, `{slot.target.prose}`
- Semver, sha256 digests, booleans, `none`
- Comparison operators: `<`, `>`, `<=`, `>=`, `!=`, `==`
- `#` line comments

Indentation rules + folding markers wire `on`/`prompt`/`unknown`/`clarify` to `end` blocks so VSCode auto-indents inside them and offers fold gutters.

### Matrix KVX grammar (`.kvx`)

Section headers, k=v pairs, decision IDs (`D1`–`D18`, `R1`–`R9`, `Q1`–`Q11`, `A1`–`A9`, `S1`–`S8`), session refs (`sess#14`), phase refs (`phase14_status`), spec section refs (`§13.4`), `matrix://` URIs, file:line refs, paths, SHA256 hashes, ISO dates, durations, byte sizes, percentages, unicode operators, status markers (`[DONE]`, `[DEFERRED]`, `[CLOSED]`, `[BLOCKED]`, …), positive/negative/state vocab, pipe and `>` separators, backticks, strings, `#` comments.

### Icon theme: "Matrix / Paxeer Icons"

**Paxeer-branded slots:**
- `.kvx` files (and `MATRIX.CTX`) → **blue** Paxeer X
- `.mtx` files (and `SKILL.mtx`) → **white** Paxeer X
- Directories literally named `cortex` (any case) → white Paxeer X
- Directories literally named `MCL` (any case) → blue Paxeer X

**Language icon pack** — 77 brand marks from [devicon](https://github.com/devicons/devicon) covering Go, Rust, TypeScript, JavaScript (+ React `.tsx/.jsx`), Python, Solidity, C/C++, C#, Java, Kotlin, Swift, Ruby, PHP, Dart, Elixir, Erlang, Clojure, Haskell, Perl, Lua, Julia, R, Nim, Groovy, Bash, PowerShell, HTML, CSS, Sass, Vue, Svelte, Astro, Flutter, JSON, YAML, Markdown, TOML (rust mark), SQL, GraphQL, WASM, plus build/lock files (`package.json`, `Cargo.toml`, `go.mod`, `tsconfig.json`, `hardhat.config.ts`, `tailwind.config.js`, `vite.config.ts`, `next.config.js`, `nuxt.config.ts`, `pyproject.toml`, `Gemfile`, `pom.xml`, `build.gradle`, …) and folders (`node_modules`, `.git`, `.github`, `contracts`, `components`, `pages`, `app`, `src`, `dist`, `build`, `target`, `vendor`, `migrations`, `kubernetes`, `terraform`, `aws`, …).

Brand marks shipped as black-on-transparent (Rust, GitHub, Next.js, Tauri, Bun, Deno, Remix, Flask, Nuxt, Vercel, Express, Three.js, Jest, Actix, Anaconda, Apple) get their root SVG fill rewritten to `#cccccc` so they remain visible on dark VSCode themes.

## Install (no Marketplace required)

### Option A — install from a packaged `.vsix`

```bash
code --install-extension matrix-kvx-0.1.0.vsix
```

### Option B — install in-place from the source folder

1. Copy the entire `matrix-kvx/` folder into your VSCode extensions directory:
   - **macOS / Linux:** `~/.vscode/extensions/`
   - **Windows:** `%USERPROFILE%\.vscode\extensions\`
2. Restart VSCode.

### Option C — package it yourself

```bash
npm i -g @vscode/vsce
cd matrix-kvx
vsce package
code --install-extension matrix-kvx-0.1.0.vsix
```

## Activate the icons

Syntax highlighting activates automatically as soon as you open a `.kvx` file. The icon theme is opt-in (VSCode only runs one icon theme at a time):

`Command Palette → "Preferences: File Icon Theme" → Matrix / Paxeer Icons`

If you'd rather keep your existing icon theme (e.g. Material Icon Theme) and only want the `.kvx` association, that's already wired via the `languages.icon` field — VSCode will show the Paxeer X on `.kvx` files even when a different icon theme is active. The custom folder icons for `cortex/` / `MCL/`, however, only apply when this icon theme is selected (that's a VSCode limitation, not the extension's).

## Customizing colors

The grammars emit semantic-ish TextMate scopes. To tune colors for your theme, add `editor.tokenColorCustomizations` to your `settings.json`:

```json
{
  "editor.tokenColorCustomizations": {
    "textMateRules": [
      /* — shared — */
      { "scope": "support.type.protocol.matrix.kvx, support.type.protocol.matrix.mtx",
        "settings": { "foreground": "#004CED", "fontStyle": "bold" } },
      { "scope": "string.unquoted.uri.matrix.kvx, string.unquoted.uri.matrix.mtx",
        "settings": { "foreground": "#90CAF9" } },

      /* — .kvx — */
      { "scope": "entity.name.section.kvx",                "settings": { "foreground": "#004CED", "fontStyle": "bold" } },
      { "scope": "variable.parameter.key.kvx",             "settings": { "foreground": "#4FC3F7" } },
      { "scope": "markup.inserted.status.done.kvx",        "settings": { "foreground": "#00C853", "fontStyle": "bold" } },
      { "scope": "markup.changed.status.pending.kvx",      "settings": { "foreground": "#FFB300" } },
      { "scope": "markup.deleted.status.blocked.kvx",      "settings": { "foreground": "#FF5252" } },
      { "scope": "constant.numeric.hex.hash.kvx",          "settings": { "foreground": "#9575CD" } },
      { "scope": "support.constant.decision-prefix.kvx",   "settings": { "foreground": "#FF8A65", "fontStyle": "bold" } },
      { "scope": "constant.numeric.decision-id.kvx",       "settings": { "foreground": "#FFAB91" } },

      /* — .mtx — */
      { "scope": "entity.name.section.mtx",                "settings": { "foreground": "#004CED", "fontStyle": "bold" } },
      { "scope": "keyword.declaration.slot.mtx",           "settings": { "foreground": "#C792EA", "fontStyle": "bold" } },
      { "scope": "variable.parameter.slot-name.mtx",       "settings": { "foreground": "#82AAFF" } },
      { "scope": "support.type.domain.mtx",                "settings": { "foreground": "#FFCB6B" } },
      { "scope": "support.type.scalar.mtx",                "settings": { "foreground": "#FFCB6B", "fontStyle": "italic" } },
      { "scope": "support.type.enum.mtx",                  "settings": { "foreground": "#FFCB6B" } },
      { "scope": "constant.other.enum-member.mtx",         "settings": { "foreground": "#F78C6C" } },
      { "scope": "keyword.control.block.on.mtx",           "settings": { "foreground": "#C792EA", "fontStyle": "bold" } },
      { "scope": "keyword.control.block.end.mtx",          "settings": { "foreground": "#C792EA" } },
      { "scope": "keyword.control.block.mtx",              "settings": { "foreground": "#C792EA" } },
      { "scope": "keyword.operator.resolve-arrow.mtx",     "settings": { "foreground": "#89DDFF", "fontStyle": "bold" } },
      { "scope": "support.function.cortex.mtx",            "settings": { "foreground": "#82AAFF", "fontStyle": "italic" } },
      { "scope": "variable.language.cortex.mtx",           "settings": { "foreground": "#004CED" } },
      { "scope": "variable.language.slot.mtx",             "settings": { "foreground": "#004CED" } },
      { "scope": "support.constant.verb.mtx",              "settings": { "foreground": "#F07178", "fontStyle": "bold" } },
      { "scope": "constant.language.severity.mtx",         "settings": { "foreground": "#FF5252" } },
      { "scope": "constant.language.action.mtx",           "settings": { "foreground": "#FF5252" } },
      { "scope": "constant.language.suggestion.mtx",       "settings": { "foreground": "#FFB300" } },
      { "scope": "constant.language.failure-reason.mtx",   "settings": { "foreground": "#FF5252", "fontStyle": "italic" } },
      { "scope": "constant.language.seed-policy.mtx",      "settings": { "foreground": "#82AAFF" } },
      { "scope": "constant.language.determinism.mtx",      "settings": { "foreground": "#82AAFF" } },
      { "scope": "variable.parameter.prompt-role.mtx",     "settings": { "foreground": "#C3E88D", "fontStyle": "bold" } },
      { "scope": "support.type.protocol.did.mtx",          "settings": { "foreground": "#004CED", "fontStyle": "bold" } },
      { "scope": "variable.language.interpolation.builtin.mtx",
        "settings": { "foreground": "#F78C6C", "fontStyle": "bold" } },
      { "scope": "constant.numeric.semver.mtx",            "settings": { "foreground": "#FFAB91" } }
    ]
  }
}
```

## File layout

```
matrix-kvx/
├── package.json                       # extension manifest (registers .kvx + .mtx)
├── language-configuration.json        # .kvx — comments + brackets
├── language-configuration-mtx.json    # .mtx — block-aware indent/folding
├── syntaxes/
│   ├── kvx.tmLanguage.json            # .kvx grammar
│   └── mtx.tmLanguage.json            # .mtx grammar (MatrixScript)
├── icons/
│   ├── matrix-icon-theme.json         # icon theme manifest
│   ├── kvx-file.svg                   # blue Paxeer X — .kvx files
│   ├── mtx-file.svg                   # white Paxeer X — .mtx files
│   ├── folder-cortex{,-open}.svg      # white Paxeer X — cortex/ dirs
│   ├── folder-mcl{,-open}.svg         # blue Paxeer X — MCL/ dirs
│   ├── file.svg                       # neutral fallback
│   ├── folder{,-open}.svg             # neutral fallback
│   └── lang/                          # 77 devicon brand marks
└── images/
    └── icon.svg / icon.png            # extension marketplace icon
```

## Credits

- Language brand marks: [devicon](https://github.com/devicons/devicon) (MIT) — Konpa et al.
- Paxeer X marks: yours.

## License

MIT — extension code, syntax grammar, language configuration, and icon theme manifest.
The devicon SVGs under `icons/lang/` retain their original MIT license.
