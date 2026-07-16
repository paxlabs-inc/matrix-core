/**
 * Runs / intents resource module.
 *
 * Maps the daemon's /intents/* surface (proxied through the router)
 * onto the UI `Run` and `Receipt` types. Every function here returns
 * UI-shaped data so hooks stay thin.
 */
import { apiFetch, apiSend } from '@/lib/api/client'
import type {
  IntentSummary,
  PagedResponse,
  AsyncStartResponse,
  AsyncJob,
  IntentAttestation,
} from '@/lib/api/types'
import {
  intentToRun,
  attestationToReceipt,
  intentsToStats,
  intentsToActivity,
} from '@/lib/api/mappers'
import type { Run, Receipt, Stats, ActivityDay } from '@/lib/matrix-data'
import type { AskResponse } from '@/lib/construct/types.gen'

export interface ListRunsOptions {
  cursor?: string
  limit?: number
  state?: IntentSummary['state']
  signal?: AbortSignal
}

export interface RunsPage {
  runs: Run[]
  raw: IntentSummary[]
  nextCursor?: string
  total?: number
}

/** Fetch a single page of intents and map to UI Run shape. */
export async function listRuns(opts: ListRunsOptions = {}): Promise<RunsPage> {
  const qs = new URLSearchParams()
  if (opts.cursor) qs.set('cursor', opts.cursor)
  if (opts.limit) qs.set('limit', String(opts.limit))
  if (opts.state) qs.set('state', opts.state)
  const q = qs.toString()
  const data = await apiFetch<PagedResponse<IntentSummary>>(`/intents${q ? `?${q}` : ''}`, {
    signal: opts.signal,
  })
  return {
    runs: (data.items ?? []).map(intentToRun),
    raw: data.items ?? [],
    nextCursor: data.next_cursor,
    total: data.total,
  }
}

/** Fetch every intent up to `cap` so the dashboard can compute global
 *  stats + activity. Keeps a hard ceiling so we don't iterate forever
 *  on a misbehaving daemon. */
export async function listAllRuns(cap = 200, signal?: AbortSignal): Promise<RunsPage> {
  let cursor: string | undefined
  const acc: IntentSummary[] = []
  for (let pages = 0; pages < 10 && acc.length < cap; pages++) {
    const page = await listRuns({ cursor, limit: 50, signal })
    acc.push(...page.raw)
    if (!page.nextCursor || page.raw.length === 0) break
    cursor = page.nextCursor
  }
  const trimmed = acc.slice(0, cap)
  return { runs: trimmed.map(intentToRun), raw: trimmed }
}

/** Pull the signed attestation envelope for a completed intent. Returns
 *  null when the intent has not been signed (still running, failed
 *  before /attest, etc.). */
export async function getRunReceipt(
  id: string,
  prose: string,
  endedAt?: string,
  signal?: AbortSignal,
): Promise<Receipt | null> {
  try {
    const att = await apiFetch<IntentAttestation>(
      `/intents/${encodeURIComponent(id)}/attestation`,
      { signal },
    )
    return attestationToReceipt(id, prose, att, endedAt)
  } catch (e) {
    // 404 = no signed envelope yet — that's expected, not an error.
    if (isNotFound(e)) return null
    throw e
  }
}

export interface DispatchInput {
  prose: string
  verb?: string
  skill?: string
  /** Optional clarify slot pre-fills, e.g. {"target": "Q2"}. */
  slotValues?: Record<string, string>
  /** Pin to a specific cortex Goal id for cost roll-ups. */
  goalId?: string
}

/** POST /messages/async — start a new run. Returns the immediately-
 *  available pointers (intent_id + URLs). */
export async function dispatchRun(input: DispatchInput): Promise<AsyncStartResponse> {
  return apiSend<AsyncStartResponse>('/messages/async', {
    prose: input.prose,
    verb: input.verb,
    skill: input.skill,
    slot_values: input.slotValues,
    goal_id: input.goalId,
  })
}

/** Poll an async job to terminal state. Used after dispatch to confirm
 *  completion + surface the final result body. */
export async function getAsyncJob(id: string, signal?: AbortSignal): Promise<AsyncJob> {
  return apiFetch<AsyncJob>(`/messages/async/${encodeURIComponent(id)}`, { signal })
}

/** POST /intents/:id/cancel — best-effort cancel. */
export async function cancelRun(id: string, reason?: string): Promise<void> {
  await apiSend<unknown>(`/intents/${encodeURIComponent(id)}/cancel`, { reason })
}

/**
 * POST /halt — the global kill switch ("Stop all").
 *
 * Interrupts EVERY live Neo run this (single-tenant) daemon is driving — the
 * user's own runaway respawns / parallel conversations — and reports how many
 * were signalled. Distinct from the per-run stop (cancelRun / the composer's
 * stop toggle): this is the deliberate escape hatch when the model is slow or a
 * storm of respawns is underway. Idempotent — halting with nothing live returns
 * `{ ok: true, halted: 0 }`.
 */
export async function haltAll(): Promise<{ ok: boolean; halted: number }> {
  return apiSend<{ ok: boolean; halted: number }>('/halt', undefined, { retries: 0 })
}

export interface GateAnswerInput {
  approved: boolean
  answer?: string
  correction?: string
}

/** POST /intents/:id/gates/:nid/answer — respond to an in-walk human gate. */
export async function answerGate(
  intentId: string,
  nodeId: string,
  answer: GateAnswerInput,
): Promise<void> {
  await apiSend<unknown>(
    `/intents/${encodeURIComponent(intentId)}/gates/${encodeURIComponent(nodeId)}/answer`,
    answer,
  )
}

/**
 * POST /intents/:id/asks/:ask_id/answer — the Construct Ask back-channel.
 *
 * Delivers a typed human response (choose / input / confirm / sign / upload)
 * to a parked construct_render(kind=ask) tool call, resuming the agent with it
 * as an INPUT. The daemon validates the response against the Ask's contract and
 * emits an answered patch that settles the rendered control.
 */
export async function answerAsk(
  intentId: string,
  askId: string,
  response: AskResponse,
): Promise<void> {
  await apiSend<unknown>(
    `/intents/${encodeURIComponent(intentId)}/asks/${encodeURIComponent(askId)}/answer`,
    response,
  )
}

export interface DashboardSnapshot {
  runs: Run[]
  raw: IntentSummary[]
  stats: Stats
  activity: ActivityDay[]
}

/** One-shot fetch used by the overview hook to populate cards + the
 *  activity timeline + the in-flight list in a single round trip. */
export async function getDashboardSnapshot(signal?: AbortSignal): Promise<DashboardSnapshot> {
  const page = await listAllRuns(200, signal)
  return {
    runs: page.runs,
    raw: page.raw,
    stats: intentsToStats(page.raw),
    activity: intentsToActivity(page.raw),
  }
}

function isNotFound(e: unknown): boolean {
  return Boolean((e as { status?: number })?.status === 404)
}
