/**
 * Automatrix resource module — mirrors the daemon's authenticated
 * /automatrix/* control surface (neo/internal/server/automatrix_routes.go).
 *
 * Automatrix is Neo's proactive "surprise task" feature: while the user is
 * idle Neo picks up a helpful, non-financial thing they mentioned in passing,
 * completes it end-to-end, and pings them with the result. This module exposes
 * the opt-in toggle, the pending-opportunity management queue, and the
 * completion inbox to the client. It carries the RESULT, never the protocol —
 * no Chronos/alarm/marker/cortex jargon leaks through these types.
 *
 * getAutomatrixSettings returns null when the daemon reports the control is
 * unavailable (503) — older daemons without the feature wired — so the UI can
 * hide the section rather than surface an error.
 */
import { apiFetch, apiSend, ApiError } from '@/lib/api/client'

/** GET/PUT /automatrix/settings — the per-user opt-in and alarm liveness. */
export interface AutomatrixSettings {
  enabled: boolean
  alarm_live: boolean
  tasks_today: number
}

/** One pending management-queue opportunity. `financial` items can only be
 *  approved for a normal, gated run — never auto-run. */
export interface AutomatrixQueueItem {
  uri: string
  summary: string
  rationale: string
  financial: boolean
  confidence: number
  status: string
  created_at?: string
}

/** One durable completion inbox record ("Neo finished something for you"). */
export interface AutomatrixInboxItem {
  id: string
  opportunity_summary: string
  result_summary: string
  conversation_id: string
  created_at: string
  read: boolean
}

export interface AutomatrixInbox {
  items: AutomatrixInboxItem[]
  unread: number
}

/** Result of approving a financial opportunity for a normal, gated run. */
export interface AutomatrixApproveResult {
  ok: boolean
  uri: string
  conversation_id: string
  intent_id: string
  events_url: string
  poll_url: string
}

/** The opt-in + alarm state, or null when the daemon has no Automatrix
 *  control wired (503) — the section is then hidden. */
export async function getAutomatrixSettings(
  signal?: AbortSignal,
): Promise<AutomatrixSettings | null> {
  try {
    return await apiFetch<AutomatrixSettings>('/automatrix/settings', { signal })
  } catch (e) {
    if (e instanceof ApiError && (e.status === 503 || e.status === 404)) return null
    throw e
  }
}

/** Toggle the opt-in. ON creates the single AUTOMATRIX alarm; OFF cancels it. */
export async function setAutomatrixEnabled(enabled: boolean): Promise<AutomatrixSettings> {
  return apiSend<AutomatrixSettings>('/automatrix/settings', { enabled }, { method: 'PUT' })
}

/** The pending opportunity queue (eligible + financial). */
export async function getAutomatrixQueue(signal?: AbortSignal): Promise<AutomatrixQueueItem[]> {
  const r = await apiFetch<{ items?: AutomatrixQueueItem[] }>('/automatrix/queue', { signal })
  return r.items ?? []
}

/** Drop an opportunity from the queue (status -> dismissed). */
export async function dismissOpportunity(uri: string): Promise<void> {
  await apiSend('/automatrix/queue/dismiss', { uri })
}

/** Approve a (typically financial) opportunity for a normal, gated,
 *  user-initiated run. Never runs it on the autonomous surface. */
export async function approveOpportunity(uri: string): Promise<AutomatrixApproveResult> {
  return apiSend<AutomatrixApproveResult>('/automatrix/queue/approve', { uri })
}

/** The completion inbox and its unread badge count. */
export async function getAutomatrixInbox(signal?: AbortSignal): Promise<AutomatrixInbox> {
  const r = await apiFetch<AutomatrixInbox>('/automatrix/inbox', { signal })
  return { items: r.items ?? [], unread: r.unread ?? 0 }
}

/** Mark one completion record read (clears it from the unread badge). */
export async function markInboxRead(id: string): Promise<void> {
  await apiSend('/automatrix/inbox/read', { id })
}
