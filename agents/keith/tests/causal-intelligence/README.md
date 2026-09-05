# Causal intelligence qualification

`scripts/qualification/causal_intelligence.py verify --task 1.1 --spec spec/keith-causal-intelligence/spec.kvx`
loads the versioned `manifests/1.1.json` and **executes** every declared required
case. Evidence is written into a new `evidence/causal-intelligence/<task>/<run-id>`
directory. An existing result file is never an input. Missing manifests,
unavailable journeys, zero/mismatched counts, skipped required tests, nonzero
exits, timeouts, stale artifacts, and changing source/configuration/binary files
all return non-success. Exit codes are 0 passed, 1 failed cases, 2 invalid or
unstable proof, and 3 blocked cases.

The initial manifest requires both infrastructure checks and the real isolated
Keith baseline. Missing binaries or an unavailable provider return non-success.
Running infrastructure self-tests does not qualify Keith behavior. Later task
manifests must be added with their actual
tests; an absent future manifest is a failure, not an inferred pass.

## Manifest interface

Each manifest has `schema_version: 1`, `task`, nonempty `source_paths`, optional
`config_paths` and `binary_paths`, an ordered `required_case_ids` list, and matching
`cases`. Paths to sources and nonsecret configuration must remain in the
repository; binaries may use absolute external build paths. Include every
implementation/test source affecting the claim, not just the entry script.

Each case declares `id`, `kind`, argv as an array, `expected_case_count` greater
than zero, and a bounded `timeout_seconds`. Supported kinds:

- `unittest`: invoke Python `-m unittest`; its real summary must match the count,
  with no skipped, expected-failure, or unexpected-success required cases.
- `cargo_test`: focused `cargo test -p <package> --locked`; successful executed
  cases must match the count and none may be ignored. Filtered-out tests do not
  count toward the declared proof.
- `cargo_clippy`: focused strict `cargo clippy -p <package> --all-targets
  --no-deps --locked -- -D warnings`; one executed check, not a test count.
- `dependency_policy`: `cargo dependency-policy`; one check, not runtime proof.
- `unavailable`: an explicit reason and positive expected count; always blocks.

All cargo commands receive `CARGO_INCREMENTAL=0` and a disposable external
`CARGO_TARGET_DIR`, cleaned after the invocation. Commands are executed directly,
never through a shell. Do not put credentials in argv, configuration artifacts,
test names, or diagnostic artifacts. Provider credentials remain in the
environment; raw subprocess output is used transiently for framework summaries
and is not copied into evidence. The evidence retains exits, command/executable
identity, times, bounded-output hash/size, counts, and source/config/binary hashes.

## Runtime baseline handoff

Parent-owned `test_runtime_baseline.py` contains the two real unittest cases
(ordinary turn and evidence ingestion/paraphrase/context observation). Its
manifest declares both cases and their runtime source, binary, and config paths.
Build the four baseline binaries into `/tmp/keith-causal-build` before invoking
the runner; test setup repeats the current-source build before launch. A changed
binary invalidates that invocation and requires a fresh run with stable inputs.
An unavailable provider fails or skips the required test, which this runner
treats as non-success.

Each child receives `KEITH_QUALIFICATION_RUN_ID`,
`KEITH_QUALIFICATION_CASE_ID`, `KEITH_QUALIFICATION_SOURCE_DIGEST`, and
`KEITH_QUALIFICATION_ARTIFACT_DIR`. Cases may declare `required_artifacts` as
relative JSON paths under that new artifact directory. A required artifact must
be nonempty, freshly written, and contain exactly matching `run_id`, `case_id`,
and `source_digest` values. JSON manifests/artifacts are limited to 2 MiB and
subprocess output to 8 MiB. Its claimed status never substitutes for test
execution. Test assertions must inspect actual observations and owning stores;
the runner cannot infer the honesty of arbitrary test code.

Hashing a current binary establishes file identity, **not build provenance**.
Runtime cases must establish that their launched binaries were produced by a
successful current-source build, preserving the executed build command and
source identity. A manually authored receipt or pre-existing `passed` JSON
cannot prove that. Qualification must distinguish a Python/test-framework
executable from the actual Keith daemon/worker binaries it launches.

Run only runner infrastructure checks with:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover \
  -s tests/causal-intelligence -p test_qualification_runner.py -v
```
