'use client'

/**
 * Receipts — derive the signed-receipt list from completed intents,
 * lazily fetching the actual `intent.attest` body for each one when
 * the user opens the receipts pane.
 */
import { useQueries, useQuery } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import { getRunReceipt } from '@/lib/api/runs'
import { useDashboardData } from '@/hooks/api/useDashboardData'
import type { Receipt } from '@/lib/matrix-data'

export interface UseReceiptsResult {
  receipts: Receipt[]
  loading: boolean
  error: string | null
}

export function useReceipts(): UseReceiptsResult {
  const dash = useDashboardData()
  const completed = dash.runs.filter((r) => r.status === 'completed')

  const queries = useQueries({
    queries: completed.map((run) => ({
      queryKey: qk.runAttest(run.id),
      queryFn: ({ signal }: { signal?: AbortSignal }) =>
        getRunReceipt(run.id, run.title, run.completedAt, signal),
      staleTime: 60_000,
    })),
  })

  const receipts: Receipt[] = []
  for (const q of queries) {
    if (q.data) receipts.push(q.data)
  }
  const errs = queries
    .map((q) => (q.error instanceof Error ? q.error.message : null))
    .filter(Boolean)
    .join('; ')
  return {
    receipts,
    loading: queries.some((q) => q.isLoading),
    error: errs || null,
  }
}

/** Single-receipt lookup — used by the receipt sheet on demand. */
export function useReceipt(runId: string | null) {
  return useQuery({
    queryKey: qk.runAttest(runId ?? ''),
    queryFn: ({ signal }) => {
      if (!runId) return null
      return getRunReceipt(runId, runId, undefined, signal)
    },
    enabled: Boolean(runId),
  })
}
