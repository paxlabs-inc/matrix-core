# Copyright © 2026 Sidiora Labs.
#
# run.py — the live-probe runner. Dispatches the Neo battery against the
# deployed API, follows each SSE stream, grades the result, classifies any
# failure as INFRA | HARNESS | PRODUCT, and writes a JSON + a human report.
#
# Usage (from inside the testkit directory):
#   MATRIX_API=<base-url> MATRIX_JWT="<bearer>" python3 run.py
#   MATRIX_API=... MATRIX_JWT=... python3 run.py --only identity,injection
#   MATRIX_API=... MATRIX_JWT=... python3 run.py --case halluc_fake_token
#   python3 run.py --list
#
# Env:
#   MATRIX_JWT   (required unless --list)  Supabase bearer token
#   MATRIX_API   base URL of the deployment under test
#                (default http://localhost:8080)
#   MATRIX_ORIGIN  CORS Origin header (default http://localhost:3000)
#
# Flags:
#   --agent neo|cody      which surface (default neo; cody uses /cody prefix)
#   --only c1,c2          restrict to categories
#   --case id[,id]        restrict to case ids
#   --delay S             seconds between cases (default 3)
#   --hard-timeout S      per-turn hard cap (default 300)
#   --danger              include money/state-mutating cases (needs go-ahead)
#   --list                print the battery and exit
#   --out DIR             report dir (default /tmp/liveprobe)

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from dataclasses import asdict

from battery import neo_cases, Case
from probe import run_turn, RunResult

BASE_PATHS = {"neo": "", "cody": "/cody"}

INFRA_SIGNS = [
    "502", "503", "504", "cloudflare", "attention required", "bad gateway",
    "gateway timeout", "service unavailable", "econnrefused", "connection refused",
    "no route to host", "temporarily unavailable", "context deadline exceeded",
]


def classify(res: RunResult, hard_failed: bool) -> str:
    """infra | harness | product | ok"""
    blob = f"{res.transport_error}\n{res.dispatch_body}".lower()
    # Transport / dispatch layer problems.
    if res.transport_error or res.dispatch_status not in (200, 202):
        if any(s in blob for s in INFRA_SIGNS) or res.dispatch_status in (429, 500, 502, 503, 504):
            return "infra"
        if res.transport_error.startswith(("dispatch:", "events connect:", "read:")):
            # Ambiguous transport failure with no infra signature: still infra
            # (the network path failed), not the agent's logic.
            return "infra"
        if res.transport_error in ("idle_timeout", "hard_timeout"):
            # Stream stalled with no terminal: agent hung (non-convergence),
            # unless literally zero events arrived (then it's infra).
            return "product" if res.events else "infra"
        return "harness"
    # Dispatched fine but never reached a terminal.
    if not res.had_terminal():
        return "product" if res.events else "infra"
    if res.terminal_status == "failed":
        return "product"
    return "product" if hard_failed else "ok"


def grade(res: RunResult, case: Case) -> dict:
    checks = []
    for index, check in enumerate(case.checks):
        try:
            checks.append(check(res))
        except Exception as exc:  # a broken grader is never a product failure
            return {
                "verdict": "HARNESS",
                "classification": "harness",
                "hard_failures": [],
                "advisory_failures": [],
                "machine_reason": {
                    "kind": "grader_exception",
                    "check_index": index,
                    "error": f"{type(exc).__name__}: {exc}",
                },
                "checks": [
                    {"name": c.name, "passed": c.passed, "severity": c.severity,
                     "detail": c.detail}
                    for c in checks
                ],
            }
    hard_fail = [c for c in checks if not c.passed and c.severity == "hard"]
    advisory_fail = [c for c in checks if not c.passed and c.severity == "advisory"]
    hard_failed = len(hard_fail) > 0
    klass = classify(res, hard_failed)
    if klass == "ok":
        verdict = "PASS" if not advisory_fail else "WARN"
    elif klass == "infra":
        verdict = "INFRA"
    elif klass == "harness":
        verdict = "HARNESS"
    else:
        verdict = "FLAW"
    return {
        "verdict": verdict,
        "classification": klass,
        "machine_reason": {
            "kind": "check_failure" if hard_fail else (
                "transport_or_protocol" if klass in ("infra", "harness") else "none"
            ),
            "hard_checks": [c.name for c in hard_fail],
        },
        "hard_failures": [c.name for c in hard_fail],
        "advisory_failures": [c.name for c in advisory_fail],
        "checks": [{"name": c.name, "passed": c.passed, "severity": c.severity,
                    "detail": c.detail} for c in checks],
    }


def run_case(base: str, token: str, case: Case, base_path: str,
             hard_timeout: float) -> tuple[RunResult, dict]:
    if case.turns:
        conv = None
        last = None
        for i, msg in enumerate(case.turns):
            last = run_turn(base, token, case.id, msg, conversation_id=conv,
                            base_path=base_path, hard_timeout=hard_timeout)
            conv = last.conversation_id or conv
            if last.transport_error and not last.events:
                break
            if i < len(case.turns) - 1:
                time.sleep(2)
        res = last
    else:
        res = run_turn(base, token, case.id, case.prompt, base_path=base_path,
                       hard_timeout=hard_timeout)
    return res, grade(res, case)


def select(cases: list, args) -> list:
    out = cases
    if not args.danger:
        out = [c for c in out if not c.danger]
    if args.only:
        cats = {s.strip() for s in args.only.split(",")}
        out = [c for c in out if c.category in cats]
    if args.case:
        ids = {s.strip() for s in args.case.split(",")}
        out = [c for c in out if c.id in ids]
    return out


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Live adversarial probe of deployed Centra AI agents.")
    ap.add_argument("--agent", choices=["neo", "cody"], default="neo")
    ap.add_argument("--only", default="")
    ap.add_argument("--case", default="")
    ap.add_argument("--delay", type=float, default=3.0)
    ap.add_argument("--hard-timeout", type=float, default=300.0)
    ap.add_argument("--danger", action="store_true")
    ap.add_argument("--list", action="store_true")
    ap.add_argument("--out", default="/tmp/liveprobe")
    args = ap.parse_args(argv)

    cases = select(neo_cases(), args)

    if args.list:
        for c in cases:
            d = " [DANGER]" if c.danger else ""
            print(f"{c.id:26s} {c.category:16s} {c.severity:8s}{d}  {c.expect}")
        print(f"\n{len(cases)} cases")
        return 0

    token = os.environ.get("MATRIX_JWT", "").strip()
    if not token:
        print("ERROR: set MATRIX_JWT to a valid Supabase bearer token.", file=sys.stderr)
        return 2
    base = os.environ.get("MATRIX_API", "http://localhost:8080").rstrip("/")
    base_path = BASE_PATHS[args.agent]

    os.makedirs(args.out, exist_ok=True)
    stamp = time.strftime("%Y%m%dT%H%M%S")
    json_path = os.path.join(args.out, f"{args.agent}_{stamp}.json")
    md_path = os.path.join(args.out, f"{args.agent}_{stamp}.md")

    print(f"# live-probe {args.agent} against {base}{base_path}  ({len(cases)} cases)\n")
    records = []
    counts = {"PASS": 0, "WARN": 0, "FLAW": 0, "INFRA": 0, "HARNESS": 0}
    for i, case in enumerate(cases):
        print(f"[{i+1}/{len(cases)}] {case.id} ({case.category}) …", flush=True)
        res, gr = run_case(base, token, case, base_path, args.hard_timeout)
        counts[gr["verdict"]] = counts.get(gr["verdict"], 0) + 1
        rec = {
            "id": case.id, "category": case.category, "severity": case.severity,
            "expect": case.expect, "prompt": case.prompt or case.turns,
            "grade": gr,
            "result": {
                "verdict": gr["verdict"],
                "intent_id": res.intent_id,
                "conversation_id": res.conversation_id,
                "dispatch_status": res.dispatch_status,
                "terminal_status": res.terminal_status,
                "latency_s": res.latency_s,
                "transport_error": res.transport_error,
                "final_answer": res.final_answer,
                "tools": [asdict(t) for t in res.tools],
                "verdict_gate": asdict(res.verdict),
                "reasoning_excerpt": res.reasoning_text[:600],
                "event_count": len(res.events),
            },
        }
        records.append(rec)
        mark = {"PASS": "  ok", "WARN": "warn", "FLAW": "FLAW", "INFRA": "infr",
                "HARNESS": "harn"}[gr["verdict"]]
        extra = ""
        if gr["hard_failures"]:
            extra = "  fails=" + ",".join(gr["hard_failures"])
        elif gr["verdict"] in ("INFRA", "HARNESS"):
            extra = "  " + (res.transport_error or res.dispatch_body[:80])
        print(f"      -> {mark}  {res.latency_s}s  answer={res.final_answer[:70]!r}{extra}")
        if i < len(cases) - 1:
            time.sleep(args.delay)

    summary = {"agent": args.agent, "base": base + base_path, "when": stamp,
               "counts": counts, "total": len(cases)}
    with open(json_path, "w") as fh:
        json.dump({"summary": summary, "records": records}, fh, indent=2)

    _write_markdown(md_path, summary, records)

    print("\n===== SUMMARY =====")
    print(json.dumps(counts, indent=2))
    print(f"JSON:   {json_path}")
    print(f"REPORT: {md_path}")
    flaws = [r for r in records if r["grade"]["verdict"] == "FLAW"]
    if flaws:
        print(f"\n{len(flaws)} FLAW(s):")
        for r in flaws:
            print(f"  - {r['id']} ({r['category']}/{r['severity']}): "
                  f"{r['grade']['hard_failures']}  answer={r['result']['final_answer'][:80]!r}")
    return 0


def _write_markdown(path: str, summary: dict, records: list) -> None:
    lines = [f"# Live probe — {summary['agent']} — {summary['when']}",
             f"Target: `{summary['base']}`  ", f"Counts: `{summary['counts']}`", ""]
    order = {"FLAW": 0, "INFRA": 1, "HARNESS": 2, "WARN": 3, "PASS": 4}
    for r in sorted(records, key=lambda x: order.get(x["grade"]["verdict"], 9)):
        res = r["result"]
        gr = r["grade"]
        lines.append(f"## [{gr['verdict']}] {r['id']} — {r['category']} ({r['severity']})")
        lines.append(f"- expect: {r['expect']}")
        lines.append(f"- classification: **{gr['classification']}**  latency: {res['latency_s']}s  "
                     f"terminal: `{res['terminal_status']}`  tools: {[t['tool'] for t in res['tools']]}")
        if gr["hard_failures"]:
            lines.append(f"- HARD failures: `{gr['hard_failures']}`")
        if gr["advisory_failures"]:
            lines.append(f"- advisory: `{gr['advisory_failures']}`")
        if res["transport_error"]:
            lines.append(f"- transport_error: `{res['transport_error']}`")
        prompt = r["prompt"] if isinstance(r["prompt"], str) else " || ".join(r["prompt"])
        lines.append(f"- prompt: {prompt[:300]}")
        lines.append(f"- answer: {res['final_answer'][:500]}")
        lines.append("")
    with open(path, "w") as fh:
        fh.write("\n".join(lines))


if __name__ == "__main__":
    raise SystemExit(main())
