/**
 * Memory resource — Neo's exposed, READ-ONLY working memory.
 *
 * Neo keeps a durable, typed memory (the cortex): preferences, facts,
 * decisions, events, and more, learned across every conversation. The daemon
 * exposes the slice that is safe to surface in the UI through the /memory/*
 * routes (proxied through the Neo server), and the client renders it on the
 * Timeline page.
 *
 * These endpoints are strictly READ-ONLY here: the agent's memory is never
 * editable by a human. This module intentionally exposes no write/delete
 * function — the daemon's POST /memory write surface is for onboarding only and
 * is not wired to the client.
 *
 * Mirrors executor/cmd/mcl-execute/daemon_memory_routes.go:
 *   GET  /memory/recent?limit=      newest-first across all types
 *   GET  /memory/types              distinct type counts
 *   GET  /memory?type=&near=&limit= filtered find
 *   POST /memory/search             body-driven complex query
 */
import { apiFetch, apiSend } from '@/lib/api/client'

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
  if (!near && types.length === 0) {
    return listRecentMemories(input.limit ?? 100, input.signal)
  }
  const data = await apiSend<ListEnvelope>(
    '/memory/search',
    {
      near: near || undefined,
      type: types.length > 0 ? types : undefined,
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
