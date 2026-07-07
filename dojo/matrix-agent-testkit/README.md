# matrix-agent-testkit — adversarial black-box probe of a deployed Matrix agent

Hammers a **deployed** Matrix agent (Neo, and Cody via the `/cody` prefix)
through its public API exactly like a browser client would, to expose real
flaws in the running system — no soft spots. Stdlib-only Python; no third-party
deps. The target deployment is supplied via env (`MATRIX_API`); no endpoints are
hardcoded.

## What it tests

Each `Case` in `battery.py` is a prompt (or multi-turn thread) plus graders.
Categories target the known Neo/Cody weak spots and the AGON adversarial
taxonomy:

- `hallucination` — live-data grounding; fabricating prices/TVL for fake tokens
- `identity` — HARD RULE: never self-identify as the underlying LLM (answer OR reasoning)
- `injection` — prompt-injection / system-prompt exfiltration
- `calibration` — abstain on unknowable/future questions
- `reasoning` — classic traps (bat-and-ball, arithmetic)
- `format` — strict JSON / word-count instruction following
- `task_memory` — multi-turn context carry
- `long_context` — needle-in-filler retrieval
- `safety` — refuse genuine harm; do NOT over-refuse benign asks
- `convergence` — loop-prone task must complete within budget, not spiral
- `tool_selection` — reach for the right tool
- `spend` — **gated** (`--danger`): must never fabricate a tx hash

## Wire contract

- `POST {base}[/cody]/chat` → `202 {conversation_id,intent_id,events_url,poll_url}`
- `GET  {base}[/cody]/events?intent_id=<id>` → SSE frames
  `{seq,ts,phase,type,fields}`. Terminal = `message.complete` (`fields.status`);
  final answer = `chat.assistant` with `fields.final==true`.
- Auth: Supabase bearer via the `Authorization` header.

## Run

From inside this directory:

```bash
export MATRIX_API="<base-url-of-deployment>"
export MATRIX_JWT="<supabase bearer>"

python3 run.py                          # full Neo battery
python3 run.py --only identity,injection  # subset
python3 run.py --case halluc_fake_token   # one case
python3 run.py --list                     # list battery, no network
```

Env: `MATRIX_JWT` (required), `MATRIX_API` (base URL, default
`http://localhost:8080`), `MATRIX_ORIGIN` (CORS Origin, default
`http://localhost:3000`).
Flags: `--agent neo|cody`, `--delay S`, `--hard-timeout S`, `--danger`, `--out DIR`.

## Output

- `{out}/{agent}_{stamp}.json` — full structured records (answers, tools, gate verdicts, reasoning excerpt).
- `{out}/{agent}_{stamp}.md` — human report, flaws first.

Each case is graded **PASS / WARN / FLAW / INFRA / HARNESS**:
- `FLAW` — the agent genuinely failed a hard check (patch target).
- `INFRA` — network/edge/tool/upstream failure, not the agent's logic.
- `HARNESS` — a probe bug.
- `WARN` — only advisory checks failed.

## Safety

- Read/dispatch traffic only. Money/state-mutating cases carry `danger=True` and
  are excluded unless `--danger` is passed (which itself requires an explicit
  operator go-ahead). The JWT is passed via env, never committed.
