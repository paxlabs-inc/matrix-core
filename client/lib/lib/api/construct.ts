/**
 * Construct rehydration resource — the read-only backfill the shell hits on a
 * cold open so the environment reappears "as the user left it".
 *
 * This is the client half of the ONE new read path the design adds
 * (`GET /construct/state`); it is NOT a new agent→client wire path. Live
 * surfaces continue to ride the existing chat SSE transport (see
 * `lib/api/events.ts`); this endpoint only backfills the durable frame log a
 * disconnected client missed, so a reopened conversation replays through the
 * same reducer the live feed uses.
 *
 * Mirrors executor/cmd/mcl-execute/daemon_construct_routes.go
 * (`GET /construct/state?conversation_id=<id>&since_seq=<n>`).
 */
import { apiFetch } from '@/lib/api/client'
import type { StateResponse } from '@/lib/construct/types.gen'

/**
 * fetchConstructState GETs a conversation's durable surface frames for
 * rehydration.
 *
 *   - `conversationId` scopes the read strictly to that conversation (the
 *     daemon refuses cross-conversation access).
 *   - `sinceSeq` (optional catch-up cursor, default 0) asks the server for only
 *     frames with `seq` strictly greater than it — a reconnecting client
 *     backfills only what it is missing. `last_seq` in the response is always
 *     the newest frame seq across the full set (before the `since_seq` filter),
 *     so the client can always advance its live cursor.
 *
 * Returns the ordered (oldest-first) frames plus `last_seq`. Throws `ApiError`
 * on a non-OK response (the caller degrades to an empty workspace + live tail).
 */
export async function fetchConstructState(
  conversationId: string,
  sinceSeq = 0,
  signal?: AbortSignal,
): Promise<StateResponse> {
  const qs = new URLSearchParams({ conversation_id: conversationId })
  if (sinceSeq > 0) qs.set('since_seq', String(sinceSeq))
  return apiFetch<StateResponse>(`/construct/state?${qs.toString()}`, { signal })
}
