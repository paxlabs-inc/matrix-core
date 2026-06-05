---
name: paxeer-watch
description: Set an autonomous on-chain watch or scheduled action on Paxeer via the Scheduler precompile (0x0905). Confirms the target/method with a read, builds calldata, computes the execute block from the live head, and schedules with a gas deposit. Never reports a job scheduled without a real tx hash + jobId.
origin: Matrix/Paxeer
---

# Paxeer Watch

"Do this later / on a schedule" — autonomous on-chain actions via the
**Scheduler precompile (0x0905)**.

- **`deliver` (schedule)** — confirm the target + method (read), build calldata
  (`encode_call`), compute `executeAtBlock` from the live head (`chain_info`),
  and `schedule_job` with a gas deposit.
- **`monitor` (manage)** — `jobs_pending` + `job_status` to list jobs;
  `cancel_job` / `reschedule_job` only when explicitly authorized.

## Anti-fake + safety mandate (the planner MUST follow)

1. READ the target contract/method before scheduling — never schedule a guessed
   call.
2. Compute the execute block from the real chain head; never invent a block/time.
3. Every schedule/cancel/reschedule MUST cite a `tx_hash` (and `jobId` for a
   schedule) from a wired tool result.
4. Unbounded or value-moving actions without a clear cap STOP to ask. The
   scheduled job **re-runs under the same wallet policy** at execution time, so a
   future value move stays gated.

## Reporting

Plain prose: what was scheduled, the execute block + approx time, the deposit,
and the `tx_hash` + `jobId`. Persist each job as an `Event`.
