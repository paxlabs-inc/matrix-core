# tools/skills

Matrix-side utilities for the skill catalog. Chain-neutral; no Paxeer coupling.

## Scripts

### `port_from_dev.sh`

One-shot port of `development/skills/` → `matrix/skills/`.

- Drop set: off-wedge language packs, Orderly SDK, healthcare, enterprise verticals, vendor-coupled (claude-api/devfleet, ECC, openclaw), Matrix-replaced (storage-basicmemory, opennote-vault), Tier 0 dev-meta (using-superpowers, skill-comply).
- Defer set: tracked in `PORT_MANIFEST.json` but not copied. Decision pending.
- Adapt set: copied but flagged in `INDEX.json` for MCL+cortex rewrite later.
- Everything else: copied as-is.

Idempotent if `matrix/skills/` is empty. Re-running overwrites copied files but does not delete drops/defers that may have been manually added.

```bash
bash tools/skills/port_from_dev.sh
```

### `build_index.py`

Generates `matrix/skills/INDEX.md` (human-readable) and `INDEX.json` (machine-readable) from frontmatter of every `SKILL.md`. Reads `PORT_MANIFEST.json` for keep/adapt status.

```bash
python3 tools/skills/build_index.py
```

Re-run after editing or adding skills.

## TODO (S1 enforcement, per research/05-skills-and-tools.md)

- [ ] `validate.py` — frontmatter schema check + body's 6 canonical sections (§16 S1).
- [ ] `publish.py` — content-hash + signed manifest registration (§16 S4, S7).
- [ ] `find.py` — verb + nearest-neighbor + salience-ranked skill discovery (§13).
- [ ] Eventually rewrite in Go and merge into `protocol/compiler/`.
