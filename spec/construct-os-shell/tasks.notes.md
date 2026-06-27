## Notes

- Tasks marked with `*` are optional test sub-tasks and can be skipped for a faster MVP; core
  implementation and wiring tasks are never optional.
- The MVP slice (tasks 1–9) is independently shippable: it delivers persistence + rehydration, a
  live activity Timeline, one level of descent, frame inversion on one adapter, the shared model,
  and a verified-live Ask back-channel — then proves the side-channel (D11) invariant holds.
- Reuse-vs-new is explicit: tasks 1.1 and 4.3 generalize/extend existing code (`neo/internal/trace`,
  `store.ts`); the 8 renderers and the `construct/{schema,transport,projection,backchannel}` packages
  are reused untouched; only `surfacestore`, the read route, the shared model, the feed hydrate path,
  and the shell adapters are new.
- Each task references the requirement clauses (R1–R17) and/or the design correctness property it
  implements; property tests are placed close to the code they validate to catch errors early.
- No deployment tasks: production redeploy is Andrew-gated and out of scope; no `git commit`/`push`
  on the dev box.
