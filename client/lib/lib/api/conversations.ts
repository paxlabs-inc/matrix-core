/**
 * Conversations resource — durable Liaison chat threads.
 *
 * The daemon persists every chat turn (user message + the agent's
 * closing answer) on its snapshotted volume, keyed by conversation_id.
 * These endpoints let the client browse past threads and reopen any of
 * them, so chat history survives reloads, new chats, suspend, and
 * redeploy — the structured human↔agent record lives in the DB, not the
 * browser.
 *
 * Mirrors executor/cmd/mcl-execute/daemon_conversations_routes.go.
 */
import { apiFetch } from '@/lib/api/client'

/** One turn of a conversation as persisted by the daemon. */
export interface ConversationTurn {
  role: 'user' | 'assistant'
  text: string
  /** The run that produced an assistant turn (absent for direct replies). */
  intent_id?: string
  ts: string
}

/** Full (bounded) turn log for one conversation. */
export interface ConversationRecord {
  conversation_id: string
  title?: string
  turns: ConversationTurn[]
  updated: string
  /**
   * The authoritative in-flight run id for this conversation, or "" once the
   * run has settled. Populated by Neo's GET /conversations/{id} (F1). A
   * non-empty value means a refresh / hard-refresh / wipe-cookies-and-relogin
   * should immediately subscribe(replay:true) to reattach the live stream
   * without waiting for the /messages/async/<id> poll. Older daemons / the
   * legacy proxied store omit this; the client must tolerate `undefined`.
   */
  live_run?: string
}

/** Compact sidebar entry returned by GET /conversations. */
export interface ConversationSummary {
  conversation_id: string
  title: string
  preview: string
  turn_count: number
  updated: string
  /** Workbench project tag (absent for untagged dashboard threads). */
  project?: string
}

interface ListResponse {
  items: ConversationSummary[]
}

/** GET /conversations — every thread, newest-first. An optional project id
 *  scopes the list server-side to one workbench project's history. */
export async function listConversations(
  signal?: AbortSignal,
  project?: string,
): Promise<ConversationSummary[]> {
  const qs = project ? `?project=${encodeURIComponent(project)}` : ''
  const data = await apiFetch<ListResponse>(`/conversations${qs}`, { signal })
  return data.items ?? []
}

/** GET /conversations/:id — full turn log for one thread. */
export async function getConversation(
  id: string,
  signal?: AbortSignal,
): Promise<ConversationRecord> {
  return apiFetch<ConversationRecord>(`/conversations/${encodeURIComponent(id)}`, { signal })
}

/**
 * One persisted workspace SSE frame — the durable record of a tool step / web
 * search / generated media / Construct surface / swarm event that built "Neo's
 * Computer". Shape mirrors the live SSE Event, so the same reducer rebuilds the
 * workspace on reopen. Mirrors `trace.Event` (Go).
 */
export interface TraceEvent {
  seq: number
  ts?: string
  phase?: string
  type: string
  fields?: Record<string, unknown>
}

/**
 * GET /conversations/:id/trace — the durable workspace timeline (F3).
 *
 * The live "Neo's Computer" (animated tool steps, source cards, media,
 * Construct surfaces, Agent-Swarm windows) is streamed as SSE events that live
 * only in the in-memory broker (a 512-event buffer dropped ~2 min after the run
 * ends). The daemon ALSO persists the workspace-relevant slice per run, so a
 * reopened thread rebuilds the workspace instead of showing an empty computer.
 *
 * `intent_id` is the run this workspace belongs to (the in-flight run, else the
 * conversation's most recent run); `live_run` mirrors the detail envelope so the
 * caller can decide whether to also subscribe for live updates; `events` are the
 * frames, oldest-first. Older daemons without this route return an empty list
 * (the client degrades to a text-only thread).
 */
export interface ConversationTrace {
  intent_id: string
  live_run: string
  events: TraceEvent[]
}

/** GET /conversations/:id/trace — durable workspace timeline for reopen. */
export async function getConversationTrace(
  id: string,
  signal?: AbortSignal,
): Promise<ConversationTrace> {
  return apiFetch<ConversationTrace>(`/conversations/${encodeURIComponent(id)}/trace`, { signal })
}
