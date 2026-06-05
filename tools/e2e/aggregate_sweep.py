#!/usr/bin/env python3
"""aggregate_sweep.py — roll up a sweep directory produced by run_sweep.sh.

Walks <sweep-root>/iterations/iter-NNN/<ts>/ and parses:
  - TOPLEVEL.jsonl                  top-level assert + cross-run.{AB,AC}
  - <tag>/transcript.jsonl per sub-run

Event names match cmd/mcl-e2e transcript.go output (verified empirically):
  compile.intent.hashed   intent_hash, intent_id, verb, objects
  compile.llm.complete    ms (compile latency), frame_len, slots, unknowns
  plan.built              plan_hash, plan_id, json_bytes
  attest.complete         weights_updated, affected, skipped, learn_seq, prev_w, new_w
  replay.rebuild.complete pre_overall_root, post_overall_root, salience_bumps_applied
  plan.tool.dispatch      tool, side_effect, node_id
  plan.tool.result        is_error, ms (tool latency), tool, node_id, result_preview
  run.complete            intent_hash, pre_root, post_root, lifecycle, walk_errors, weights_upd

Emits:
  <sweep-root>/summary.md
  <sweep-root>/summary.csv
  <sweep-root>/summary.json

Pure stdlib (json, os, statistics, csv).
"""

import csv
import json
import os
import statistics
import sys
from collections import Counter
from pathlib import Path


def load_jsonl(path):
    if not path.exists():
        return
    with path.open() as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                yield json.loads(line)
            except json.JSONDecodeError:
                return


def find_e2e_run_dir(iter_dir):
    subs = [p for p in iter_dir.iterdir() if p.is_dir() and p.name != "iterations"]
    if not subs:
        return None
    subs.sort(key=lambda p: p.stat().st_mtime, reverse=True)
    return subs[0]


def parse_subrun(tag_dir):
    """Parse <tag>/transcript.jsonl into a per-sub-run stats dict."""
    s = {
        "intent_hash": None, "intent_id": None, "verb": None,
        "compile_ms": None, "frame_len": None, "slots": None, "unknowns": None,
        "plan_hash": None, "plan_id": None, "plan_json_bytes": None,
        "attest_affected": None, "attest_skipped": None, "attest_learn_seq": None,
        "weights_updated": None,
        "pre_overall_root": None, "post_overall_root": None,
        "salience_bumps": None,
        "lifecycle": None, "walk_errors": None,
        "tool_calls": 0, "tool_errors": 0, "tool_ms": [],
        "tools_used": Counter(),
        "lifecycle_terminal": None,
    }
    for ev in load_jsonl(tag_dir / "transcript.jsonl"):
        t = ev.get("type")
        if t == "compile.intent.hashed":
            s["intent_hash"] = ev.get("intent_hash")
            s["intent_id"] = ev.get("intent_id")
            s["verb"] = ev.get("verb")
        elif t == "compile.llm.complete":
            s["compile_ms"] = ev.get("ms")
            s["frame_len"] = ev.get("frame_len")
            s["slots"] = ev.get("slots")
            s["unknowns"] = ev.get("unknowns")
        elif t == "plan.built":
            s["plan_hash"] = ev.get("plan_hash")
            s["plan_id"] = ev.get("plan_id")
            s["plan_json_bytes"] = ev.get("json_bytes")
        elif t == "attest.complete":
            s["attest_affected"] = ev.get("affected")
            s["attest_skipped"] = ev.get("skipped")
            s["attest_learn_seq"] = ev.get("learn_seq")
            s["weights_updated"] = ev.get("weights_updated")
        elif t == "replay.rebuild.complete":
            s["pre_overall_root"] = ev.get("pre_overall_root")
            s["post_overall_root"] = ev.get("post_overall_root")
            s["salience_bumps"] = ev.get("salience_bumps_applied")
        elif t == "plan.tool.dispatch":
            tool = ev.get("tool", "")
            s["tools_used"][tool] += 1
        elif t == "plan.tool.result":
            s["tool_calls"] += 1
            if ev.get("is_error"):
                s["tool_errors"] += 1
            ms = ev.get("ms")
            if isinstance(ms, (int, float)):
                s["tool_ms"].append(ms)
        elif t == "run.complete":
            s["lifecycle"] = ev.get("lifecycle")
            s["walk_errors"] = ev.get("walk_errors")
            # post_root + pre_root are mirrored here too; replay.rebuild is canonical.
    s["replay_ok"] = (
        s["pre_overall_root"] is not None
        and s["pre_overall_root"] == s["post_overall_root"]
    )
    return s


def parse_iter(iter_dir):
    stats = {
        "iter": iter_dir.name,
        "found": False,
        "asserts_pass": 0,
        "asserts_fail": 0,
        "runs": {},
        "ab_hash_equal": None,
        "ab_root_equal": None,
    }
    e2e_root = find_e2e_run_dir(iter_dir)
    if e2e_root is None:
        return stats
    stats["found"] = True
    stats["e2e_root"] = str(e2e_root)

    for ev in load_jsonl(e2e_root / "TOPLEVEL.jsonl"):
        t = ev.get("type")
        if t == "assert":
            if ev.get("ok"):
                stats["asserts_pass"] += 1
            else:
                stats["asserts_fail"] += 1
        elif t == "cross-run.AB":
            stats["ab_hash_equal"] = ev.get("hash_match")
            stats["ab_root_equal"] = ev.get("root_match")

    for tag in ("A", "B", "C"):
        sub = e2e_root / tag
        if sub.is_dir():
            stats["runs"][tag] = parse_subrun(sub)
    return stats


def pct(num, denom):
    if denom == 0:
        return "0.0%"
    return f"{(num / denom) * 100:.1f}%"


def latency_table(samples):
    if not samples:
        return {"n": 0}
    s = sorted(samples)
    return {
        "n": len(s),
        "min": s[0],
        "p50": int(statistics.median(s)),
        "p95": s[max(0, int(len(s) * 0.95) - 1)],
        "p99": s[max(0, int(len(s) * 0.99) - 1)],
        "max": s[-1],
        "mean": round(statistics.mean(s), 1),
    }


def summarize(sweep_root):
    cfg_path = sweep_root / "sweep.config.json"
    cfg = json.loads(cfg_path.read_text()) if cfg_path.exists() else {}

    iter_root = sweep_root / "iterations"
    if not iter_root.exists():
        print(f"no iterations/ under {sweep_root}", file=sys.stderr)
        return 2

    iters = sorted([p for p in iter_root.iterdir() if p.is_dir()])
    rows = [parse_iter(it) for it in iters]

    total = len(rows)
    found = sum(1 for r in rows if r["found"])
    clean = sum(1 for r in rows if r["found"] and r["asserts_fail"] == 0)
    ab_h_eq = sum(1 for r in rows if r["ab_hash_equal"] is True)
    ab_h_t = sum(1 for r in rows if r["ab_hash_equal"] is not None)
    ab_r_eq = sum(1 for r in rows if r["ab_root_equal"] is True)
    ab_r_t = sum(1 for r in rows if r["ab_root_equal"] is not None)

    per_tag = {}
    for tag in ("A", "B", "C"):
        replay = [r["runs"][tag]["replay_ok"]
                  for r in rows if tag in r["runs"]
                  and r["runs"][tag]["pre_overall_root"] is not None]
        compile_ms = [r["runs"][tag]["compile_ms"]
                      for r in rows if tag in r["runs"]
                      and r["runs"][tag]["compile_ms"]]
        tool_ms = []
        tool_count = 0
        tool_errors = 0
        weights_upd = 0
        weights_total = 0
        intent_hashes = Counter()
        plan_hashes = Counter()
        post_roots = Counter()
        all_tools = Counter()
        lifecycle_counts = Counter()
        for r in rows:
            sub = r["runs"].get(tag)
            if not sub:
                continue
            tool_ms.extend(sub["tool_ms"])
            tool_count += sub["tool_calls"]
            tool_errors += sub["tool_errors"]
            if sub["weights_updated"] is not None:
                weights_total += 1
                if sub["weights_updated"]:
                    weights_upd += 1
            if sub["intent_hash"]:
                intent_hashes[sub["intent_hash"]] += 1
            if sub["plan_hash"]:
                plan_hashes[sub["plan_hash"]] += 1
            if sub["post_overall_root"]:
                post_roots[sub["post_overall_root"]] += 1
            for tool, n in sub["tools_used"].items():
                all_tools[tool] += n
            if sub["lifecycle"]:
                lifecycle_counts[sub["lifecycle"]] += 1
        per_tag[tag] = {
            "replay_ok": sum(replay),
            "replay_total": len(replay),
            "replay_rate": pct(sum(replay), len(replay)) if replay else "n/a",
            "compile_ms": latency_table(compile_ms),
            "tool_call_count": tool_count,
            "tool_error_count": tool_errors,
            "tool_error_rate": pct(tool_errors, tool_count) if tool_count else "n/a",
            "tool_ms": latency_table(tool_ms),
            "weights_updated_rate": pct(weights_upd, weights_total) if weights_total else "n/a",
            "distinct_intent_hashes": len(intent_hashes),
            "distinct_plan_hashes": len(plan_hashes),
            "distinct_post_roots": len(post_roots),
            "top_intent_hashes": intent_hashes.most_common(3),
            "top_plan_hashes": plan_hashes.most_common(3),
            "top_post_roots": post_roots.most_common(3),
            "tool_usage_top10": all_tools.most_common(10),
            "lifecycle_path_count": len(lifecycle_counts),
        }

    headline = {
        "config": cfg,
        "total_iterations": total,
        "iterations_parsed": found,
        "iterations_clean": clean,
        "clean_rate": pct(clean, found) if found else "n/a",
        "ab_intent_hash_equal_rate": pct(ab_h_eq, ab_h_t) if ab_h_t else "n/a",
        "ab_overall_root_equal_rate": pct(ab_r_eq, ab_r_t) if ab_r_t else "n/a",
        "per_tag": per_tag,
    }

    (sweep_root / "summary.json").write_text(json.dumps(
        {"headline": headline, "iterations": rows},
        indent=2, default=str))

    # CSV
    csv_path = sweep_root / "summary.csv"
    with csv_path.open("w", newline="") as f:
        w = csv.writer(f)
        w.writerow([
            "iter", "found", "asserts_pass", "asserts_fail",
            "AB_hash_eq", "AB_root_eq",
            "A_intent_hash", "A_compile_ms", "A_tool_calls", "A_tool_errors",
            "A_post_root", "A_replay_ok", "A_weights_updated",
            "B_intent_hash", "B_compile_ms", "B_post_root", "B_replay_ok",
            "C_intent_hash", "C_compile_ms", "C_post_root", "C_replay_ok",
        ])
        for r in rows:
            ra = r["runs"].get("A", {})
            rb = r["runs"].get("B", {})
            rc = r["runs"].get("C", {})
            w.writerow([
                r["iter"], r["found"], r["asserts_pass"], r["asserts_fail"],
                r["ab_hash_equal"], r["ab_root_equal"],
                ra.get("intent_hash"), ra.get("compile_ms"),
                ra.get("tool_calls"), ra.get("tool_errors"),
                ra.get("post_overall_root"), ra.get("replay_ok"),
                ra.get("weights_updated"),
                rb.get("intent_hash"), rb.get("compile_ms"),
                rb.get("post_overall_root"), rb.get("replay_ok"),
                rc.get("intent_hash"), rc.get("compile_ms"),
                rc.get("post_overall_root"), rc.get("replay_ok"),
            ])

    # Markdown
    md = sweep_root / "summary.md"
    with md.open("w") as f:
        f.write(f"# Sweep summary — `{sweep_root.name}`\n\n")
        f.write("## Headline\n\n")
        f.write(f"- **Total iterations:** {total}\n")
        f.write(f"- **Parsed:** {found}\n")
        f.write(f"- **Clean (0 assertion failures):** {clean} ({headline['clean_rate']})\n")
        f.write(f"- **A==B Intent.Hash equal:** {ab_h_eq}/{ab_h_t} ({headline['ab_intent_hash_equal_rate']}) — D11 informational\n")
        f.write(f"- **A==B OverallRoot equal:** {ab_r_eq}/{ab_r_t} ({headline['ab_overall_root_equal_rate']})\n\n")

        f.write("## Per sub-run\n\n")
        f.write("| Tag | Replay OK | Compile ms (p50/p95/max) | Tool calls | Tool err | Weights upd | Distinct intent_hashes | Distinct plan_hashes | Distinct post_roots |\n")
        f.write("|---|---|---|---|---|---|---|---|---|\n")
        for tag in ("A", "B", "C"):
            t = per_tag[tag]
            cm = t["compile_ms"]
            cml = (f"{cm['p50']}/{cm['p95']}/{cm['max']}" if cm["n"] else "—")
            f.write(f"| {tag} | {t['replay_ok']}/{t['replay_total']} ({t['replay_rate']}) | {cml} | {t['tool_call_count']} | {t['tool_error_count']} ({t['tool_error_rate']}) | {t['weights_updated_rate']} | {t['distinct_intent_hashes']} | {t['distinct_plan_hashes']} | {t['distinct_post_roots']} |\n")
        f.write("\n")

        f.write("## Tool latency (sub-run A)\n\n")
        tl = per_tag["A"]["tool_ms"]
        if tl["n"]:
            f.write(f"- n={tl['n']}, min={tl['min']}, p50={tl['p50']}, p95={tl['p95']}, p99={tl['p99']}, max={tl['max']}, mean={tl['mean']} ms\n\n")
        else:
            f.write("- (no samples)\n\n")

        f.write("## Top tools (sub-run A)\n\n")
        for tool, n in per_tag["A"]["tool_usage_top10"]:
            f.write(f"- `{tool}`: {n}\n")
        f.write("\n")

        f.write("## Most-common intent hashes per tag\n\n")
        for tag in ("A", "B", "C"):
            top = per_tag[tag]["top_intent_hashes"]
            f.write(f"**{tag}:**\n")
            if not top:
                f.write("- (none)\n")
            for h, n in top:
                f.write(f"- `{h[:16]}…` × {n}\n")
            f.write("\n")

        f.write("## Config\n\n```json\n")
        f.write(json.dumps(cfg, indent=2))
        f.write("\n```\n\n")
        f.write("Per-iteration detail: `summary.csv`. Full structured aggregate: `summary.json`.\n")

    print(json.dumps(headline, indent=2, default=str))
    return 0


def main():
    if len(sys.argv) != 2:
        print("usage: aggregate_sweep.py <sweep-root>", file=sys.stderr)
        return 2
    return summarize(Path(sys.argv[1]))


if __name__ == "__main__":
    sys.exit(main())
