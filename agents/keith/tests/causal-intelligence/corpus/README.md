# Frozen qualification corpus v1

This directory freezes infrastructure and future proof obligations. It does not
record passing C01–C20/B01–B10 behavior. `cases.json` defines 30 regressions and
their variants; `task-map.json` covers all 44 tasks, relevant packages, required
runtime levels, and the non-success policy for missing tests. Future owning task
manifests must replace planned test descriptions with actual executable cases and
exact counts before qualification.

`evaluation-policy.json` fixes the ablation arms, matched controls, metric
denominators, initial gates, three-replicate protocol, uncertainty method, and
optional enablement criteria. Reconstruction compares verified success in E
versus D. Pattern retention compares later correct decisions with the best
explicit-importance/recency/frequency comparator selected on development only.
Positive paired-family 95% confidence bounds and the specified success, factual
support, and intervention limits are required. Negative/inconclusive experiments
retain the simpler accepted configuration.

The historical baseline is the actual task 1.1 run with 24 infrastructure checks
and two real Keith/provider tests. Its immutable operator copy preserves the
original bytes and source identity. It is a smoke baseline, not a statistical
estimate for the frozen ordinary-task comparison. Earlier failed runs remain in
the ordinary evidence directory.

## Operator bundle and candidate boundary

Private fixture contents live only in the separately restored operator bundle
at `/root/keith-evaluation/causal-intelligence/v1`, with owner-only directory/file
permissions. `commitments.json` records names, versions, counts, and hashes.
`freeze.json` is duplicated into the operator bundle and binds public data files.
The current metadata revision updates only task ownership for tasks 1.2, 2.1,
and 2.5. Its predecessor is retained as `public-freeze-r1.json` in the operator
bundle; sealed experiment inputs and evaluation gates remain byte-identical.
Missing files, changed hashes, inappropriate permissions, symlinks, or changed
fixtures during execution fail qualification. Distribution/backup/restore of
the bundle is a separate operator action. There is no plaintext Git fallback.
Do not copy fixture contents into prompts, memories, logs, or handoffs.

The sealed bundle includes:

- 200 low-overlap semantic queries over 40 independent originating facts, with
  five correlated paraphrases per fact; ranking candidates deduplicate by anchor
  ID. Content-word Jaccard is at most 0.25. This is a synthetic operational set,
  not a claim of natural-world coverage or 200 independent episodes.
- 60 ordinary-task prompts over 20 task families, with three correlated
  phrasings per family and exact file-result predicates.
- 24 chronological latent-retention scenarios over eight mechanism families:
  reversal, rare exception, scoped correction, deletion, self-caused feedback,
  work-topic confusion, delayed usefulness, and counterevidence.
- 24 reconstruction/conditional-transfer scenarios over eight families:
  incidental versus relevant variation, misleading analogy, unsupported bridges
  and details, delayed outcomes, source correction, and contradicted lessons.
- Separate validation data and the unchanged historical baseline report.

These counts describe frozen inputs, not executed evaluations. Bootstrap
uncertainty clusters by originating fact or mechanism/task family, never by
paraphrase count. Eight mechanism families are a limited initial sample; an
inconclusive confidence interval cannot justify default enablement.

`candidate-inputs.json` permits only `development.json` for candidate prompts and
lesson generation. Candidates receive a fresh explicit export, never the whole
checkout or operator directory. This matters because existing candidate shadow
creation copies/archives its supplied source tree; protected names alone do not
hide readable files. The Rust boundary tests use the existing
`IndependentEvaluator` projection and `RestrictedProcessRunner` with
`UntrustedWorkspace`. They attempt direct, symlink, and `/proc` access to the real
operator bundle. Strong isolation unavailable means candidate execution is
refused; it never permits an ordinary subprocess fallback. Boundary evidence
distinguishes actual sandbox execution from that refusal path.

The cross-domain Rust test lives in `crates/test-support/tests`; that crate's
strict checks qualify this infrastructure. An earlier check of the unchanged
`keith-self-evolution` runtime found 87 unique pre-existing Clippy diagnostics
across nine files. `preexisting-self-evolution-lints.json` preserves their counts,
source hashes, and command. Broader self-evolution package qualification remains
incomplete; this corpus task does not repair or waive that backlog.

The runner's `external_fixture_paths` field fingerprints each declared absolute
regular fixture before/after the invocation under
`source_identity.external_fixtures`; it does not export contents or mislabel
fixtures as executable binaries. The task manifest must name every consumed
operator fixture explicitly.

## Durable external target

`../operation_service.py` runs a loopback HTTP service backed by its own SQLite
database. `POST /effects` accepts `scope`, `operation_key`, `target`, and integer
`delta`; a committed request changes a named counter. Repeating the identical
key/payload returns its original receipt. A conflicting payload receives 409.
`GET /operations/<scope>/<key>` provides authoritative readback.

`--drop-ack-once` commits an effect and consumed-fault marker in one transaction,
then closes the connection without sending a response. Restart does not reset
the fault. Tests independently inspect real SQLite rows, kill/restart the
process, and submit concurrent duplicates. This proves the external target's
contract, not Keith's recovery wiring. Its `scope` is a test namespace, not a
substitute for authenticated Keith profile authority. It is not a production
service and is never exposed beyond loopback by this implementation.
