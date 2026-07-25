/**
 * Dojo resource — the disposable desktop (DOJO wave 3).
 *
 * The desktop is an ephemeral sandbox that exists only while in use; its
 * lifecycle streams to the surface as dojo.* events. This module carries the
 * panel's pull side:
 *
 *   - `getDojoSession` seeds the panel with the live session state on open
 *     (the events rebuild history; this corrects staleness after a reap).
 *   - `loadDojoFrame` fetches one live-view frame (JPEG) as an authed blob
 *     object URL. The panel polls it while visible — plain request/response,
 *     no held stream. Callers must revoke the URL when done.
 *   - `dojoTakeover` / `dojoHandback` move the control lease (req 4.2/4.3).
 *   - `sendDojoInput` passes one human input action through to the desktop
 *     while the lease is held.
 */
import { apiFetch, apiSend } from '@/lib/api/client'

export interface DojoSession {
  id: string
  conversation_id: string
  state: string
  sandbox_id?: string
  image?: string
  created_at?: string
  touched_at?: string
}

/** Read the conversation's live desktop session, or null when none (404/503
 *  from an older daemon degrade to null — the panel simply stays event-driven). */
export async function getDojoSession(conversationId: string): Promise<DojoSession | null> {
  if (!conversationId) return null
  try {
    const res = await apiFetch<{ session: DojoSession | null }>(
      `/dojo/session?conversation=${encodeURIComponent(conversationId)}`,
    )
    return res.session ?? null
  } catch {
    return null
  }
}

/** Fetch one live-view frame as an authed blob object URL (null while booting,
 *  gone, or unreachable). Caller owns revoking the URL. */
export async function loadDojoFrame(conversationId: string): Promise<string | null> {
  if (!conversationId) return null
  try {
    const res = await apiFetch<Response>(
      `/dojo/frame?conversation=${encodeURIComponent(conversationId)}`,
      { raw: true, retries: 0, timeoutMs: 30_000 },
    )
    const blob = await res.blob()
    if (!blob.type.startsWith('image/')) return null
    return URL.createObjectURL(blob)
  } catch {
    return null
  }
}

/** Turn the desktop ON — the user's power button, independent of the agent.
 *  Reattaches the live session or starts a fresh boot; the returned snapshot
 *  is the immediate state (usually provisioning on a cold boot). */
export async function dojoBoot(conversationId: string): Promise<DojoSession> {
  const res = await apiSend<{ session: DojoSession }>(
    '/dojo/boot',
    { conversation_id: conversationId },
    { method: 'POST' },
  )
  return res.session
}

/** Turn the desktop OFF: the ship-first teardown (never a discard).
 *  Idempotent — shutting down an already-off desktop is a calm no-op. */
export async function dojoShutdown(conversationId: string): Promise<null> {
  await apiSend('/dojo/shutdown', { conversation_id: conversationId }, { method: 'POST' })
  return null
}

/** Take the control lease (req 4.2): the human drives, agent actions are
 *  rejected with takeover_active until handback. */
export async function dojoTakeover(conversationId: string): Promise<DojoSession> {
  const res = await apiSend<{ session: DojoSession }>(
    '/dojo/takeover',
    { conversation_id: conversationId },
    { method: 'POST' },
  )
  return res.session
}

/** Hand the controls back (req 4.3): an explicit event; the agent re-observes
 *  before its next action. */
export async function dojoHandback(conversationId: string): Promise<DojoSession> {
  const res = await apiSend<{ session: DojoSession }>(
    '/dojo/handback',
    { conversation_id: conversationId },
    { method: 'POST' },
  )
  return res.session
}

/** One bytebotd input action passed through while the human holds the lease. */
export interface DojoInputRequest {
  action: string
  [key: string]: unknown
}

/** Send one human input action to the desktop (takeover only). Returns false
 *  when the desktop rejected it (lease not held, session gone). */
export async function sendDojoInput(
  conversationId: string,
  request: DojoInputRequest,
): Promise<boolean> {
  try {
    await apiSend('/dojo/input', { conversation_id: conversationId, request }, { method: 'POST' })
    return true
  } catch {
    return false
  }
}
