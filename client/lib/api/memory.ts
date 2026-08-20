/**
 * Memory resource — Neo's durable, typed working memory (the cortex).
 *
 * Neo keeps preferences, facts, decisions, events, and more, learned across
 * conversations. Durable personalization is OFF by default and only ever
 * populated after the user explicitly opts in (PRIV-01). This module is the
 * user's control surface: read memory, correct or delete a single record,
 * export everything, set the consent state, and manage the personalization
 * profile — all through Neo's OWN authenticated routes (Neo owns these; the
 * co-located daemon runs a separate cortex actor).
 *
 * Neo server routes (agents/neo/internal/server):
 *   GET  /memory/recent?limit=      newest-first across all types
 *   GET  /memory/types              distinct type counts
 *   POST /memory/search             body-driven complex query
 *   POST /memory/mutate             typed create/update/supersede/delete
 *   GET/PUT /memory/consent         default-off durable-memory opt-in
 *   GET  /memory/export             export every current memory as JSON
 *   DELETE /memory/delete-all       receipt-backed erasure (gated on ORACLE)
 *   GET/PUT/DELETE /personalization the single personalization profile
 */
import { apiFetch, apiSend, ApiError } from '@/lib/api/client'

/**
 * The canonical set of memory types the cortex exposes. Used to drive the
 * Timeline's type filter chips. Kept in sync with the daemon's allMemoryTypes().
 */
export const MEMORY_TYPES = [
  'Identity',
  'Fact',
  'Preference',
  'Belief',
  'Event',
  'Goal',
  'Constraint',
  'Capability',
  'Pattern',
] as const

export type MemoryType = (typeof MEMORY_TYPES)[number]

/** One exposed memory, as returned by the /memory/* routes (summary shape). */
export interface MemoryEntry {
  /** Stable content-addressed URI, e.g. "cortex://preference/abcd@3". */
  uri: string
  /** Memory type name (Preference, Fact, Decision-as-Belief, Event, …). */
  type: string
  version: number
  hash?: string
  created_at?: string
  updated_at?: string
  created_by?: string
  confidence?: number
  salience?: number
  declared_importance?: number
  tags?: string[]
  /** One-line rendering. */
  form_short?: string
  /** The human-readable rendering the UI shows (a sentence or two). */
  form_medium?: string
  tombstoned?: boolean
  provenance?: Record<string, unknown>
}

interface ListEnvelope {
  items?: MemoryEntry[]
  next_cursor?: string
  total_estimate?: number
  total?: number
}

interface TypeCountEnvelope {
  items?: { type: string; count: number }[]
}

/** GET /memory/recent — newest memories first, across all types. */
export async function listRecentMemories(
  limit = 100,
  signal?: AbortSignal,
): Promise<MemoryEntry[]> {
  const data = await apiFetch<ListEnvelope>(`/memory/recent?limit=${clampLimit(limit)}`, { signal })
  return (data.items ?? []).filter((m) => !m.tombstoned)
}

/** GET /memory/types — distinct type counts (drives the filter chips). */
export async function listMemoryTypeCounts(
  signal?: AbortSignal,
): Promise<{ type: string; count: number }[]> {
  const data = await apiFetch<TypeCountEnvelope>('/memory/types', { signal })
  return data.items ?? []
}

export interface MemorySearchInput {
  /** Free-text natural-language recall phrase (vector search when available). */
  near?: string
  /** Restrict to one or more memory types. */
  types?: string[]
  /** Bi-temporal valid-time: query what was true at this instant.
   *  ISO 8601 string. nil/undefined = now. */
  asOf?: string
  limit?: number
  signal?: AbortSignal
}

/**
 * Search exposed memories. Uses POST /memory/search (body-driven) so a
 * multi-type + free-text query fits cleanly. Falls back to newest-first when
 * the query is empty.
 */
export async function searchMemories(input: MemorySearchInput): Promise<MemoryEntry[]> {
  const near = (input.near ?? '').trim()
  const types = input.types ?? []
  if (!near && types.length === 0 && !input.asOf) {
    return listRecentMemories(input.limit ?? 100, input.signal)
  }
  const data = await apiSend<ListEnvelope>(
    '/memory/search',
    {
      near: near || undefined,
      type: types.length > 0 ? types : undefined,
      as_of: input.asOf || undefined,
      limit: clampLimit(input.limit ?? 100),
      form: 'medium',
    },
    { retries: 0, signal: input.signal },
  )
  return (data.items ?? []).filter((m) => !m.tombstoned)
}

function clampLimit(n: number): number {
  if (!Number.isFinite(n) || n <= 0) return 100
  return Math.min(200, Math.floor(n))
}

// ------------------------------------------------------------------ consent

/** The durable, auditable consent state (matches neo MemoryConsentState). */
export interface MemoryConsentState {
  enabled: boolean
  explicit: boolean
  notice_version?: string
  updated_at?: string
  updated_by?: string
  existing_data?: string
}

export interface MemoryConsentResponse {
  consent: MemoryConsentState
  /** Plain-language explanation shown before the first opt-in. */
  notice: string
}

/** GET /memory/consent — the current opt-in state plus the pre-opt-in notice. */
export async function getMemoryConsent(signal?: AbortSignal): Promise<MemoryConsentResponse> {
  return apiFetch<MemoryConsentResponse>('/memory/consent', { signal })
}

/** PUT /memory/consent — turn durable memory on or off. */
export async function setMemoryConsent(enabled: boolean): Promise<MemoryConsentResponse> {
  return apiSend<MemoryConsentResponse>(
    '/memory/consent',
    { enabled },
    { method: 'PUT', retries: 0 },
  )
}

// ------------------------------------------------------------------ mutation

interface MutationEnvelope {
  results?: { operation: string; description: string; uri?: string }[]
}

/** Delete one memory record by its URI (typed tombstone via /memory/mutate). */
export async function deleteMemory(uri: string): Promise<void> {
  await apiSend<MutationEnvelope>(
    '/memory/mutate',
    { items: [{ operation: 'delete', target: { uri } }] },
    { retries: 0 },
  )
}

/** Edit one memory record's content in place (typed update via /memory/mutate). */
export async function editMemory(uri: string, type: string, content: string): Promise<void> {
  await apiSend<MutationEnvelope>(
    '/memory/mutate',
    {
      items: [
        { operation: 'update', target: { uri }, value: { type: type.toLowerCase(), content } },
      ],
    },
    { retries: 0 },
  )
}

// -------------------------------------------------------------------- export

/** GET /memory/export — the full current memory as a JSON document. */
export async function exportMemories(signal?: AbortSignal): Promise<unknown> {
  return apiFetch<unknown>('/memory/export', { signal })
}

// ---------------------------------------------------------------- delete-all

/** The receipt returned when delete-all is not yet available. */
export interface DeleteAllUnavailable {
  available: false
  error: string
  dependency: string
  alternative?: string
}

/**
 * DELETE /memory/delete-all. Full receipt-backed erasure is gated on ORACLE's
 * cryptographic-erasure pipeline; until it ships the server refuses fail-closed
 * with a 424. Returns the unavailable receipt instead of throwing so the UI can
 * show the prerequisite plainly.
 */
export async function requestDeleteAll(): Promise<DeleteAllUnavailable> {
  try {
    await apiFetch('/memory/delete-all', { method: 'DELETE', retries: 0 })
    // A future success path (once ORACLE lands) would return here.
    return { available: false, error: '', dependency: '' } as DeleteAllUnavailable
  } catch (e) {
    if (e instanceof ApiError && e.status === 424 && e.body && typeof e.body === 'object') {
      return e.body as DeleteAllUnavailable
    }
    throw e
  }
}

// ------------------------------------------------------------- personalization

export interface MediaTaste {
  liked?: string[]
  disliked?: string[]
}

export interface PersonalizationProfile {
  schema_version?: number
  interests?: string[]
  day_to_day_goals?: string[]
  media?: {
    music?: MediaTaste
    films?: MediaTaste
    shows?: MediaTaste
    books?: MediaTaste
    games?: MediaTaste
    creators?: MediaTaste
  }
  adventurousness?: string
  brief_preferences?: { length?: string; sections?: string[] }
}

export interface PersonalizationResponse {
  profile: PersonalizationProfile
  uri?: string
}

/** GET /personalization — the single saved profile. */
export async function getPersonalization(signal?: AbortSignal): Promise<PersonalizationResponse> {
  return apiFetch<PersonalizationResponse>('/personalization', { signal })
}

/** PUT /personalization — save/replace the single profile (an explicit opt-in). */
export async function savePersonalization(
  profile: PersonalizationProfile,
): Promise<PersonalizationResponse> {
  return apiSend<PersonalizationResponse>('/personalization', profile, {
    method: 'PUT',
    retries: 0,
  })
}

/** DELETE /personalization — remove the profile record. */
export async function deletePersonalization(): Promise<void> {
  await apiFetch('/personalization', { method: 'DELETE', retries: 0 })
}
