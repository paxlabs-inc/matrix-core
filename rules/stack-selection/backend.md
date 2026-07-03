# Backend Language Selection

Decision table for picking a backend language/runtime from the workload. Cite this in the
SDR. Default is the doctrine pick; deviation requires a stated, requirement-backed reason.

## Decision table

| Workload class | Default | Why | Do not default to |
|---|---|---|---|
| high-throughput backend / services | Go | Cheap goroutine concurrency, low and predictable latency, tiny static binaries, fast builds, trivial deploy; the sweet spot for network services and APIs under load | Node/TS for a CPU- or fan-out-heavy service just because the frontend is TS |
| correctness-critical / systems | Rust | Memory safety without GC, no data races by construction, exhaustive types; when a bug is a financial/safety/consensus event, the borrow checker pays for itself | Go or C++ where a whole class of memory/concurrency bugs must be impossible, not just unlikely |
| ML-adjacent / data / scientific | Python with `uv` | The entire ecosystem (numpy, pandas, torch, sklearn) lives here; `uv` makes envs fast and reproducible, fixing Python's historic packaging pain | Rewriting model/data glue in another language to avoid Python — you will reimplement the ecosystem |
| standard CRUD API behind a JS frontend | Node/TS | Type sharing with the frontend, one language across the stack, huge ecosystem; correct when throughput is ordinary and team velocity matters | Go/Rust for a plain CRUD API with no performance pressure — you pay in velocity for headroom you will not use |
| glue / scripts / automation | Python or Node/TS | Whatever the surrounding code already is; optimize for the reader, not for speed | A compiled language for a 200-line orchestration script |

## Reasoning notes

### high-throughput → Go

When the job is "handle many concurrent connections with predictable tail latency," Go is
the default for a reason: goroutines make concurrency cheap and readable, the GC is tuned
for low pause times, binaries are static and deploy anywhere, and builds are fast enough to
stay in flow. Reach for Go for gateways, proxies, ingestion services, and hot-path APIs.
Deviate to Rust only if you also need zero-GC determinism; deviate to Node only if the
service is I/O-trivial and shipping fast matters more than the ceiling.

### correctness-critical → Rust

Rust is the pick when a defect is not a bug ticket but an incident: consensus/ledger code,
cryptography, parsers on untrusted input, systems where memory corruption or a data race is
unacceptable. The type system and borrow checker turn whole bug classes into compile
errors. The cost is slower authoring and a real learning curve — justified only when
correctness dominates velocity. Do not reach for Rust for an ordinary web backend; that is
the anti-default in the other direction (headroom you pay for and never use).

### ML / data / scientific → Python with `uv`

Do not fight the ecosystem. Numerical and ML work belongs in Python because that is where
the libraries, the models, and the community are. The historical pain was environments and
packaging; `uv` resolves that — fast installs, reproducible lockfiles, drop-in for pip/
venv. Use `uv` for env and dependency management on every Python project, not just ML. See
the `toolchain-bootstrap` skill for installing `uv` on a fresh box.

### standard CRUD → Node/TS

For an ordinary API serving a JS frontend with no performance pressure, Node/TS is correct
and choosing Go or Rust is over-engineering. You get end-to-end type sharing, one language,
and maximum team velocity. Save the compiled languages for when a measured requirement — a
throughput target, a latency SLO, a correctness bar — actually asks for them.

## Deviation rules

- **Measure before you upgrade.** "This might get slow" is not a requirement. A profiled
  bottleneck or a stated SLO is. Do not pick Go/Rust on speculation.
- **Match the existing codebase.** Adding a second backend language to a service that is
  already Go or already Python needs a strong reason; a polyglot backend is a standing tax.
- **Team capability is a real constraint.** A team with no Rust experience shipping
  consensus code is its own risk. Name the tradeoff in the SDR rather than pretending it
  away.

## Anti-default guardrail

If your backend answer is "Node/TS" for every workload — the 50k-rps gateway, the ledger,
the training pipeline, and the CRUD app alike — you are not selecting, you are defaulting.
The gate catches exactly this. If changing the workload class does not change your answer,
re-derive from the table.
