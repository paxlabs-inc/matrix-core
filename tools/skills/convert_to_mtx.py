#!/usr/bin/env python3
"""
SKILL.md → SKILL.mtx converter.

Reads YAML frontmatter from each /root/matrix/skills/<slug>/SKILL.md, infers
MCL D7 verbs from name + description, and emits a baseline SKILL.mtx that
parses + validates against the MCL spec (see MCL/mtx/spec.md §11).

The full prose body of SKILL.md remains in place — SKILL.mtx is the
compiler-participating manifest. The two coexist for now (SKILL.md is the
human-authored body the executor LLM still consumes).
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

SKILLS_ROOT = Path("/root/matrix/skills")

# D7 closed verb vocab (from research/00-decisions.md + matrix.kvx).
D7_VERBS = [
    "find", "acquire", "build", "modify", "deliver",
    "analyze", "negotiate", "schedule", "monitor", "delegate",
]

# Heuristic keyword → verb mapping, evaluated against name + description.
# Order matters: more specific verbs first.
VERB_KEYWORDS: list[tuple[str, list[str]]] = [
    ("delegate",  ["delegate", "dispatch", "spawn agent", "sub-agent", "subagent", "hand off", "hand-off", "handoff"]),
    ("monitor",   ["monitor", "watch", "track", "observability", "observe", "telemetry", "metrics"]),
    ("schedule",  ["schedule", "queue", "cron", "orchestrate"]),
    ("negotiate", ["negotiate", "bargain", "haggle", "agree on terms"]),
    ("acquire",   ["acquire", "purchase", "buy ", "fetch", "obtain", "procure", "ingest", "scrape"]),
    ("deliver",   ["deliver", "deploy", "ship ", "publish", "release", "rollout", "roll out", "distribute"]),
    ("find",      ["search", "find ", "discover", "research", "lookup", "look up", "audit", "scan", "explore", "investigate", "tour", "onboard"]),
    ("analyze",   ["analyze", "analyse", "review", "evaluate", "assess", "diagnose", "debug", "benchmark", "profile", "examine", "validate", "verify", "test"]),
    ("modify",    ["modify", "edit", "update", "refactor", "fix", "patch", "improve", "optimize", "optimise", "tune", "adjust", "rename", "migrate"]),
    ("build",     ["build", "create", "implement", "construct", "scaffold", "generate", "author", "write", "design", "develop", "make", "craft", "compose", "produce", "set up", "setup", "configure", "bootstrap"]),
]

# Fallback verb when no keyword matches. analyze is the broadest and safest
# umbrella for a "skill the agent invokes" without a clear action.
FALLBACK_VERB = "analyze"

# Cap on how many primary verbs we emit per skill.
MAX_VERBS = 3

# Simple frontmatter delimiter regex.
FRONTMATTER_RE = re.compile(r"\A---\s*\n(.*?)\n---\s*\n?", re.DOTALL)

# Matches `key: value` lines (single-line values only — multi-line YAML lists
# are handled separately for `tools:`).
KV_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_-]*)\s*:\s*(.*)$")


@dataclass
class SkillSource:
    slug: str
    md_path: Path
    name: str
    description: str
    origin: str
    tools_raw: str  # original YAML value (may be empty)


def parse_frontmatter(md_text: str) -> dict[str, str]:
    """Parse minimal YAML frontmatter into a dict.
    Handles the subset used by the Matrix skill corpus: scalar key:value pairs
    with values that may continue on subsequent indented lines (folded).
    """
    m = FRONTMATTER_RE.match(md_text)
    if not m:
        return {}
    body = m.group(1)
    out: dict[str, str] = {}
    cur_key: str | None = None
    cur_val: list[str] = []
    for raw in body.splitlines():
        if not raw.strip():
            # blank line inside frontmatter – treat as space in folded value
            if cur_key is not None:
                cur_val.append("")
            continue
        kv = KV_RE.match(raw)
        if kv and not raw.startswith((" ", "\t")):
            # flush previous
            if cur_key is not None:
                out[cur_key] = "\n".join(cur_val).strip()
            cur_key = kv.group(1)
            cur_val = [kv.group(2).strip()]
        else:
            # continuation
            if cur_key is not None:
                cur_val.append(raw.strip())
    if cur_key is not None:
        out[cur_key] = "\n".join(cur_val).strip()
    return out


def load_skill(slug_dir: Path) -> SkillSource | None:
    md = slug_dir / "SKILL.md"
    if not md.is_file():
        return None
    try:
        text = md.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    fm = parse_frontmatter(text)
    if not fm:
        return None
    name = fm.get("name", slug_dir.name).strip() or slug_dir.name
    desc = fm.get("description", "").strip()
    # collapse internal whitespace runs
    desc = re.sub(r"\s+", " ", desc)
    return SkillSource(
        slug=slug_dir.name,
        md_path=md,
        name=name,
        description=desc,
        origin=fm.get("origin", "").strip(),
        tools_raw=fm.get("tools", "").strip(),
    )


def infer_verbs(src: SkillSource) -> list[str]:
    haystack = f"{src.slug} {src.name} {src.description}".lower()
    matches: list[str] = []
    for verb, keywords in VERB_KEYWORDS:
        for kw in keywords:
            if kw in haystack:
                if verb not in matches:
                    matches.append(verb)
                break
    if not matches:
        matches = [FALLBACK_VERB]
    return matches[:MAX_VERBS]


def display_name(slug: str) -> str:
    return " ".join(part.capitalize() for part in re.split(r"[-_]", slug))


def normalise_description(desc: str, slug: str) -> str:
    if not desc:
        desc = f"{display_name(slug)} skill"
    # Strip outer YAML quotes (single or double) if the entire value was quoted.
    desc = desc.strip()
    if len(desc) >= 2 and desc[0] == desc[-1] and desc[0] in ('"', "'"):
        desc = desc[1:-1]
    # Strip leading "Use ... to" / "Use when ..." stems for readability.
    desc = re.sub(r"^Use this skill to\s+", "", desc, flags=re.IGNORECASE)
    desc = re.sub(r"^Use when\s+", "", desc, flags=re.IGNORECASE)
    desc = re.sub(r"^Use this skill when\s+", "", desc, flags=re.IGNORECASE)
    desc = desc.strip()
    if not desc:
        desc = f"{display_name(slug)} skill"
    # 280 char cap (per spec §6.1).
    if len(desc) > 280:
        desc = desc[:277].rstrip() + "..."
    # strip newlines and trailing punctuation that confuses single-line value
    desc = desc.replace("\n", " ").replace("\r", " ")
    desc = re.sub(r"\s+", " ", desc).strip()
    # forbid characters that would break key=value parsing on a single line
    # (none currently — the lexer treats everything after `=` as a string until newline).
    return desc


def escape_str(s: str) -> str:
    """Escape a string literal for embedding inside SKILL.mtx double-quoted strings."""
    return s.replace("\\", "\\\\").replace('"', '\\"')


def primary_tag(slug: str) -> str:
    """Pick a single tag derived from the slug (skill folder name).
    Hyphens are replaced with spaces so multi-word slugs become multiple tags.
    """
    return slug.replace("-", " ").replace("_", " ").lower().strip()


def render_mtx(src: SkillSource, verbs: list[str]) -> str:
    slug = src.slug
    display = display_name(slug)
    desc = normalise_description(src.description, slug)
    tags = primary_tag(slug)
    primary_verb = verbs[0]
    verbs_line = " ".join(verbs)

    # Header comment + §SKILL.
    out: list[str] = []
    out.append(f"# {display} — MatrixScript skill manifest (generated from SKILL.md)")
    out.append(f"# Source: skills/{slug}/SKILL.md (frontmatter)")
    out.append("# Prose body remains in SKILL.md; this manifest is the compiler-participating surface.")
    out.append("")
    out.append("§SKILL")
    out.append(f"id={slug}")
    out.append("version=0.1.0")
    out.append(f'display="{escape_str(display)}"')
    out.append("author=did:pax:0xPLACEHOLDER")
    out.append(f'description="{escape_str(desc)}"')
    out.append(f"mcl.verbs={verbs_line}")
    out.append("determinism=seedable")
    out.append("seed_policy=per_intent")
    out.append("")

    # §INPUTS — minimal but useful.
    out.append("§INPUTS")
    out.append("slot target: ArtifactRef")
    out.append("  required")
    out.append('  hint="The artifact, system, or topic this skill should be applied to"')
    out.append("")
    out.append("slot constraints: Constraint[]")
    out.append("  optional")
    out.append('  hint="Hard constraints the skill output must satisfy"')
    out.append("")

    # §CORTEX — sensible read set.
    out.append("§CORTEX")
    out.append("reads=Fact Goal Pattern Event Constraint Preference")
    out.append(f"tags={tags}")
    out.append("")

    # §TOOLS — none by default (executor-side decides based on plan).
    out.append("§TOOLS")
    out.append("none")
    out.append("")

    # §SUB_SKILLS — none by default.
    out.append("§SUB_SKILLS")
    out.append("none")
    out.append("")

    # §PROCEDURE — emit one on-block per verb + a fallback clarify.
    out.append("§PROCEDURE")
    for v in verbs:
        sys_prompt = (
            f"You are the Matrix compiler invoking the {slug} skill: {desc} "
            f"Extract a fully-typed Frame for this '{v}' intent. "
            "Output valid JSON matching intent_frame@1. No prose. No explanation."
        )
        user_prompt = (
            "User goal: {prose}\\n\\n"
            "Cortex context (resolved memories):\\n{cortex.bundle}\\n\\n"
            "Currently resolved slots:\\n{slots}\\n\\n"
            "Pending unknowns:\\n{unknowns}\\n\\n"
            "Extract the Frame. Resolve all object references to matrix:// URIs where possible."
        )
        out.append(f"on verb={v}")
        out.append("  prompt")
        out.append(f'    system="{escape_str(sys_prompt)}"')
        out.append(f'    user="{escape_str(user_prompt)}"')
        out.append("  end")
        if v in ("build", "modify", "deliver", "analyze", "monitor", "schedule", "find", "acquire", "delegate"):
            out.append("  resolve slot.target <- cortex.find(type=Fact, near=slot.target.prose, limit=5)")
        out.append("  unknown slot.target")
        out.append("    severity=blocking")
        out.append(f'    reason="Cannot identify the target for the {slug} skill from the user\'s phrasing"')
        out.append("  end")
        out.append("end")
        out.append("")

    out.append("on confidence<0.75")
    out.append("  clarify slot.target")
    out.append(f'    prompt="What specifically should the {display} skill operate on?"')
    out.append("    type=ArtifactRef")
    out.append("    required=true")
    out.append("  end")
    out.append("end")
    out.append("")

    out.append("on unknown")
    out.append("  clarify slot.target")
    out.append('    prompt="I could not find a matching reference in your memories. Can you describe the target more specifically?"')
    out.append("    type=ArtifactRef")
    out.append("    required=true")
    out.append("  end")
    out.append("end")
    out.append("")

    # §OUTPUTS — minimal generic shape.
    out.append("§OUTPUTS")
    out.append("slot result: ArtifactRef")
    out.append("  required")
    out.append('  hint="The artifact, plan, or report produced by this skill"')
    out.append("")
    out.append("slot unknowns: Unknown[]")
    out.append("  optional")
    out.append("")

    # §FAILURE_MODES — three canonical failure paths.
    out.append("§FAILURE_MODES")
    out.append("target_not_found")
    out.append("  action=fail")
    out.append("  reason=unknown_information")
    out.append("")
    out.append("ambiguous_after_clarify")
    out.append("  action=fail")
    out.append("  reason=ambiguous_request")
    out.append("")
    out.append("policy_violation")
    out.append("  action=fail")
    out.append("  reason=policy_violation")
    out.append("")
    out.append("budget_exceeded")
    out.append("  suggest=raise_budget")
    out.append("")

    # §HASH — left empty; mtxc will populate at publish time.
    out.append("§HASH")
    out.append("v=1")
    out.append("algo=sha256_ast")
    out.append("digest=")
    out.append("")

    return "\n".join(out)


# Slugs whose SKILL.mtx is hand-authored as a canonical fixture and must not
# be overwritten by this converter (referenced by MCL test suite).
RESERVED_SLUGS = {"writing-plans"}


def iter_skill_dirs(root: Path) -> Iterable[Path]:
    for child in sorted(root.iterdir()):
        if child.is_dir() and (child / "SKILL.md").is_file():
            if child.name in RESERVED_SLUGS:
                continue
            yield child


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=str(SKILLS_ROOT), help="skills root directory")
    ap.add_argument("--force", action="store_true", help="overwrite existing SKILL.mtx")
    ap.add_argument("--dry-run", action="store_true", help="print summary, write nothing")
    ap.add_argument("--only", help="convert only this slug (debug)")
    args = ap.parse_args()

    root = Path(args.root)
    if not root.is_dir():
        print(f"error: skills root not found: {root}", file=sys.stderr)
        return 2

    written = 0
    skipped = 0
    failed = 0
    converted: list[tuple[str, list[str]]] = []
    for slug_dir in iter_skill_dirs(root):
        if args.only and slug_dir.name != args.only:
            continue
        src = load_skill(slug_dir)
        if src is None:
            print(f"skip {slug_dir.name}: no SKILL.md or unparsable frontmatter", file=sys.stderr)
            failed += 1
            continue
        out_path = slug_dir / "SKILL.mtx"
        if out_path.exists() and not args.force:
            skipped += 1
            continue
        verbs = infer_verbs(src)
        body = render_mtx(src, verbs)
        if args.dry_run:
            print(f"would write {out_path} (verbs={','.join(verbs)})")
        else:
            out_path.write_text(body, encoding="utf-8")
            written += 1
        converted.append((slug_dir.name, verbs))

    # Summary
    print(f"converted={len(converted)} written={written} skipped_existing={skipped} failed={failed}")
    if args.only:
        for slug, vs in converted:
            print(f"  {slug}: {','.join(vs)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
