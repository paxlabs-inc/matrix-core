'use client'

/**
 * useDispatch — POST a new run and optimistically prepend the result
 * to the dashboard cache so the UI updates without waiting for the
 * next list refetch.
 */
import { useCallback } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { dispatchRun, type DispatchInput } from '@/lib/api/runs'
import { qk } from '@/lib/query/keys'
import { buildStages, type Run } from '@/lib/matrix-data'
import type { DashboardSnapshot } from '@/lib/api/runs'

export interface UseDispatchResult {
  dispatch: (input: DispatchInput) => Promise<{ intentId: string }>
  isPending: boolean
  error: Error | null
}

export function useDispatch(): UseDispatchResult {
  const qc = useQueryClient()
  const mut = useMutation({
    mutationFn: async (input: DispatchInput) => {
      const r = await dispatchRun(input)
      return { intentId: r.intent_id }
    },
    onMutate: async (input) => {
      const key = qk.dashboardSnapshot()
      await qc.cancelQueries({ queryKey: key })
      const prev = qc.getQueryData<DashboardSnapshot>(key)
      const optimistic: Run = {
        id: `pending_${Date.now()}`,
        title: input.prose,
        status: 'queued',
        agentId: 'agt_research',
        toolIds: ['tool_web', 'tool_files'],
        autonomy: 'checkpoints',
        progress: 4,
        stages: buildStages(0),
        createdAt: new Date().toISOString(),
      }
      if (prev) {
        qc.setQueryData<DashboardSnapshot>(key, {
          ...prev,
          runs: [optimistic, ...prev.runs],
        })
      }
      return { prev }
    },
    onError: (_err, _input, ctx) => {
      // Rollback the optimistic insert.
      if (ctx?.prev) qc.setQueryData(qk.dashboardSnapshot(), ctx.prev)
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: qk.dashboardSnapshot() })
    },
  })

  const dispatch = useCallback(
    async (input: DispatchInput) => {
      return mut.mutateAsync(input)
    },
    [mut],
  )

  return {
    dispatch,
    isPending: mut.isPending,
    error: (mut.error as Error | null) ?? null,
  }
}
