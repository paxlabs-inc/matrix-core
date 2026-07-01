'use client'

/**
 * Automatrix hooks — the proactive opt-in, the pending-opportunity management
 * queue, and the completion inbox, over the daemon's /automatrix/* surface.
 *
 * useAutomatrixSettings resolves to `null` when the daemon has no Automatrix
 * control wired, letting the Settings UI hide the section entirely. The
 * toggle is optimistic (the Switch flips instantly, rolls back on error) and
 * refreshes the queue on enable so the user immediately sees what Neo might
 * pick up. Dismiss/approve/mark-read invalidate the affected lists.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  approveOpportunity,
  dismissOpportunity,
  getAutomatrixInbox,
  getAutomatrixQueue,
  getAutomatrixSettings,
  markInboxRead,
  setAutomatrixEnabled,
  type AutomatrixInbox,
  type AutomatrixQueueItem,
  type AutomatrixSettings,
} from '@/lib/api/automatrix'

/* ── Queries ─────────────────────────────────────────────────────────────── */

/** The per-user opt-in + alarm state, or null when unavailable. */
export function useAutomatrixSettings() {
  return useQuery<AutomatrixSettings | null>({
    queryKey: qk.automatrixSettings(),
    queryFn: ({ signal }) => getAutomatrixSettings(signal),
    staleTime: 30_000,
  })
}

/** The pending opportunity queue. Only fetched when `enabled` (the section is
 *  open / Automatrix is on) to avoid a wasted call on every settings open. */
export function useAutomatrixQueue(enabled = true) {
  return useQuery<AutomatrixQueueItem[]>({
    queryKey: qk.automatrixQueue(),
    queryFn: ({ signal }) => getAutomatrixQueue(signal),
    enabled,
    staleTime: 15_000,
  })
}

/** The completion inbox + unread badge. */
export function useAutomatrixInbox(enabled = true) {
  return useQuery<AutomatrixInbox>({
    queryKey: qk.automatrixInbox(),
    queryFn: ({ signal }) => getAutomatrixInbox(signal),
    enabled,
    staleTime: 15_000,
  })
}

/* ── Mutations ───────────────────────────────────────────────────────────── */

/** Toggle the opt-in, optimistically. On enable, refresh the queue. */
export function useSetAutomatrixEnabled() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (enabled: boolean) => setAutomatrixEnabled(enabled),
    onMutate: async (enabled) => {
      await qc.cancelQueries({ queryKey: qk.automatrixSettings() })
      const prev = qc.getQueryData<AutomatrixSettings | null>(qk.automatrixSettings())
      if (prev) {
        qc.setQueryData<AutomatrixSettings>(qk.automatrixSettings(), { ...prev, enabled })
      }
      return { prev }
    },
    onError: (_e, _enabled, ctx) => {
      if (ctx?.prev !== undefined) qc.setQueryData(qk.automatrixSettings(), ctx.prev)
    },
    onSuccess: (data) => {
      qc.setQueryData(qk.automatrixSettings(), data)
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: qk.automatrixSettings() })
      qc.invalidateQueries({ queryKey: qk.automatrixQueue() })
    },
  })
}

/** Dismiss an opportunity from the queue, optimistically removing the row. */
export function useDismissOpportunity() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (uri: string) => dismissOpportunity(uri),
    onMutate: async (uri) => {
      await qc.cancelQueries({ queryKey: qk.automatrixQueue() })
      const prev = qc.getQueryData<AutomatrixQueueItem[]>(qk.automatrixQueue())
      if (prev) {
        qc.setQueryData<AutomatrixQueueItem[]>(
          qk.automatrixQueue(),
          prev.filter((o) => o.uri !== uri),
        )
      }
      return { prev }
    },
    onError: (_e, _uri, ctx) => {
      if (ctx?.prev) qc.setQueryData(qk.automatrixQueue(), ctx.prev)
    },
    onSettled: () => qc.invalidateQueries({ queryKey: qk.automatrixQueue() }),
  })
}

/** Approve a financial opportunity for a normal, gated run. It leaves the
 *  pending queue once accepted. */
export function useApproveOpportunity() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (uri: string) => approveOpportunity(uri),
    onSettled: () => qc.invalidateQueries({ queryKey: qk.automatrixQueue() }),
  })
}

/** Mark one completion record read (clears it from the unread badge). */
export function useMarkInboxRead() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => markInboxRead(id),
    onMutate: async (id) => {
      await qc.cancelQueries({ queryKey: qk.automatrixInbox() })
      const prev = qc.getQueryData<AutomatrixInbox>(qk.automatrixInbox())
      if (prev) {
        const items = prev.items.map((r) => (r.id === id ? { ...r, read: true } : r))
        const unread = items.filter((r) => !r.read).length
        qc.setQueryData<AutomatrixInbox>(qk.automatrixInbox(), { items, unread })
      }
      return { prev }
    },
    onError: (_e, _id, ctx) => {
      if (ctx?.prev) qc.setQueryData(qk.automatrixInbox(), ctx.prev)
    },
    onSettled: () => qc.invalidateQueries({ queryKey: qk.automatrixInbox() }),
  })
}
