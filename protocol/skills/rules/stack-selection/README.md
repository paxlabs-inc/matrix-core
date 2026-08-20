# Stack Selection Doctrine

Curated defaults for choosing a stack per app class. The Stack Decision Rationale (SDR)
gate cites this doctrine; it is not a suggestion box. Pick the doctrine default unless a
concrete requirement forces a deviation, and when you deviate, say why in the SDR.

## Why this exists

Left ungoverned, stack choice collapses to whatever the model reaches for first — usually
`create-next-app` and npm — regardless of what the app actually is. That is a tell, not a
decision. A realtime chat app and a static marketing site should not resolve to the same
stack. This doctrine encodes the mapping a senior engineer would defend in review.

## The SDR gate

Before scaffolding, the gate requires a one-paragraph Stack Decision Rationale that:

1. Names the **app class** (see the tables below).
2. States the **doctrine default** for that class.
3. Either **accepts** the default, or **deviates** with a stated, concrete reason tied to a
   requirement — not taste, not familiarity, not "it's popular."

No SDR, no scaffold.

## Deviation demands stated rationale

Deviation is allowed and sometimes correct. It is never free.

- A valid reason cites a **requirement or constraint**: an existing codebase to match, a
  team's operational skill, a hard latency/throughput target, a compliance boundary, a
  library that only exists in one ecosystem.
- An invalid reason is a **preference or a reflex**: "Next is the default," "I know React
  best," "everyone uses X." These do not survive the gate.
- Record the deviation and its reason. If the same deviation recurs, the doctrine is wrong —
  fix the doctrine, don't keep re-arguing it.

## Anti-default guardrail

> If your stack choice would be identical regardless of the requirements, you did not make
> a decision — you skipped one.

Run this check on every pick. If swapping the app class (chat → marketing site →
enterprise admin) does not change your answer, you are pattern-matching on habit. Re-derive
from the tables.

## The mapping (canonical)

| App class | Stack |
|---|---|
| chat / realtime / collaborative | React Router framework mode (Remix) on Node/TS |
| content / marketing / mostly-static | Astro (or Next) on Node/TS |
| internal enterprise / forms-heavy admin | Angular on Node/TS |
| high-throughput backend / services | Go |
| correctness-critical / systems | Rust |
| ML-adjacent / data / scientific | Python with uv |
| classic web app, no strong signal | Node/TS + Vite, or Next, with npm |

Detail and reasoning:

- Frontend framework selection → [`frontend.md`](frontend.md)
- Backend language selection → [`backend.md`](backend.md)

## Bootstrapping the toolchain

A default box will not have Angular CLI, the Rust toolchain, `uv`, or Bun preinstalled. Do
not silently downgrade the stack to whatever is already on the box — that reintroduces the
anti-default failure. Install the chosen toolchain on demand; see the `toolchain-bootstrap`
skill.
