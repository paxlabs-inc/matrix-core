#!/usr/bin/env python3
"""Build matrix/skills/INDEX.md and INDEX.json from ported SKILL.md frontmatter."""
import json
import re
import sys
from pathlib import Path

SKILLS_DIR = Path("/root/matrix/skills")
MANIFEST = SKILLS_DIR / "PORT_MANIFEST.json"

FRONTMATTER_RE = re.compile(r"^---\s*\n(.*?)\n---\s*\n", re.DOTALL)

def parse_frontmatter(path: Path) -> dict:
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except Exception:
        return {}
    m = FRONTMATTER_RE.match(text)
    if not m:
        return {}
    body = m.group(1)
    out: dict = {}
    cur_key = None
    cur_buf: list[str] = []
    for line in body.splitlines():
        if not line.strip():
            continue
        # top-level "key: value" or "key: |"
        kv = re.match(r"^([A-Za-z_][\w-]*)\s*:\s*(.*)$", line)
        if kv and not line.startswith(" "):
            if cur_key is not None:
                out[cur_key] = " ".join(s.strip() for s in cur_buf).strip()
            cur_key = kv.group(1)
            v = kv.group(2)
            if v in ("|", ">", "|-", ">-"):
                cur_buf = []
            else:
                out[cur_key] = v.strip().strip('"').strip("'")
                cur_key = None
                cur_buf = []
        elif cur_key is not None and line.startswith(" "):
            cur_buf.append(line.strip())
    if cur_key is not None:
        out[cur_key] = " ".join(s.strip() for s in cur_buf).strip()
    return out

def main() -> int:
    manifest = json.loads(MANIFEST.read_text())
    keep = set(manifest["keep"])
    adapt = set(manifest["adapt"])

    entries = []
    for d in sorted(SKILLS_DIR.iterdir()):
        if not d.is_dir():
            continue
        slug = d.name
        skill_md = d / "SKILL.md"
        fm = parse_frontmatter(skill_md) if skill_md.exists() else {}
        desc = fm.get("description", "")
        # collapse + truncate
        desc = re.sub(r"\s+", " ", desc).strip()
        if len(desc) > 220:
            desc = desc[:217] + "..."
        status = "keep" if slug in keep else "adapt" if slug in adapt else "unknown"
        entries.append({
            "slug": slug,
            "status": status,
            "name": fm.get("name", slug),
            "description": desc,
            "origin": fm.get("origin", ""),
        })

    # Write INDEX.json
    (SKILLS_DIR / "INDEX.json").write_text(json.dumps({
        "v": "matrix/skill-index/0.1",
        "count": len(entries),
        "skills": entries,
    }, indent=2))

    # Write INDEX.md
    lines = [
        "# Matrix Skills Index",
        "",
        f"**Total ported:** {len(entries)} ({len([e for e in entries if e['status']=='keep'])} keep + {len([e for e in entries if e['status']=='adapt'])} adapt)",
        "",
        "**Source:** `/root/matrix/development/skills/` — see `PORT_MANIFEST.json` for full drop/defer lists.",
        "",
        "**Status legend:**",
        "- `keep` — ported near-as-is; conforms or near-conforms to matrix S1 schema",
        "- `adapt` — copied with MCL+cortex rewrite pending (see `journal/notes/01-skill-triage.md`)",
        "",
        "**Schema:** see `research/05-skills-and-tools.md` §2.3 for canonical frontmatter (S1).",
        "",
        "---",
        "",
        "## Skills",
        "",
        "| Status | Slug | Description |",
        "|---|---|---|",
    ]
    for e in entries:
        desc = e["description"].replace("|", "\\|").replace("\n", " ")
        lines.append(f"| `{e['status']}` | [`{e['slug']}`]({e['slug']}/) | {desc} |")

    lines.extend([
        "",
        "---",
        "",
        "## Notes",
        "",
        "- This index is generated. Edits go in `SKILL.md` files themselves; re-run `tools/skills/index` (TBD) to regenerate.",
        "- Skills marked `adapt` need MCL-binding frontmatter (verbs, slot_requires/produces, cortex.reads, tools[], determinism) added during the rewrite pass.",
        "- The KEEP set has legacy Claude-Code frontmatter (`name`, `description`, optionally `origin`). The validator (S1 enforcement) will flag missing fields; that's an audit pass, not a port-time concern.",
    ])

    (SKILLS_DIR / "INDEX.md").write_text("\n".join(lines))
    print(f"Wrote INDEX.md ({len(entries)} entries) and INDEX.json")
    return 0

if __name__ == "__main__":
    sys.exit(main())
