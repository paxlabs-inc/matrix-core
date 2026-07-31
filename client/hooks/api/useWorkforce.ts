'use client'

import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  getWorkforceResource,
  getWorkforceSession,
  submitWorkforceCommand,
  type WorkforceResource,
  type WorkforceResourceItem,
  type WorkforceResourcePage,
} from '@/lib/api/workforce'
import { qk } from '@/lib/query/keys'
import {
  startWorkforceEventStream,
  type WorkforceStreamState,
} from '@/lib/realtime/workforce-events'
import {
  activateSignedWorkforce,
  submitPreparedWorkOrder,
  signWorkforceCommand,
  type WorkforceCommandDraft,
} from '@/lib/workforce/control-signing'
import type { WorkforceWorkOrder } from '@/lib/api/workforce'

const resources: WorkforceResource[] = [
  'departments',
  'seats',
  'work-orders',
  'graph',
  'schedules',
  'incidents',
  'mail',
  'approvals',
  'receipts',
  'policies',
  'project-brain',
  'corrections',
  'audit-disagreements',
  'replay-lineage',
  'effect-status',
  'control-versions',
]

interface WorkforceResourceResult {
  resource: WorkforceResource
  page?: WorkforceResourcePage
  error?: Error
}

export function useWorkforceCommandCenter() {
  const queryClient = useQueryClient()
  const [streamState, setStreamState] = useState<WorkforceStreamState>('connecting')
  const [lastCursor, setLastCursor] = useState(0)
  const session = useQuery({
    queryKey: qk.workforceSession(),
    queryFn: ({ signal }) => getWorkforceSession(signal),
    staleTime: 5 * 60_000,
  })
  const resourceSnapshot = useQuery({
    queryKey: ['workforce', 'resources'],
    queryFn: async ({ signal }): Promise<WorkforceResourceResult[]> => {
      return Promise.all(
        resources.map(async (resource) => {
          try {
            return {
              resource,
              page: await getWorkforceResource(resource, { signal }),
            }
          } catch (error) {
            if (signal.aborted) throw error
            return {
              resource,
              error: error instanceof Error ? error : new Error(String(error)),
            }
          }
        }),
      )
    },
    staleTime: 10_000,
  })

  useEffect(() => {
    const stream = startWorkforceEventStream({
      after: lastCursor,
      onState: setStreamState,
      onEvent: (event) => {
        setLastCursor(event.cursor)
        void queryClient.invalidateQueries({ queryKey: ['workforce', 'resources'] })
      },
      onResync: (cursor) => {
        setLastCursor(cursor)
        void queryClient.invalidateQueries({ queryKey: ['workforce'] })
      },
    })
    return stream.close
    // The stream owns its cursor. Recreating it on each event would produce
    // parallel reconnect loops.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryClient])

  const byResource = useMemo(() => {
    const result = new Map<WorkforceResource, WorkforceResourceItem[]>()
    for (const resource of resources) result.set(resource, [])
    for (const entry of resourceSnapshot.data ?? []) {
      result.set(entry.resource, entry.page?.items ?? [])
    }
    return result
  }, [resourceSnapshot.data])

  const resourceErrors = useMemo(() => {
    const unique = new Map<string, Error>()
    for (const entry of resourceSnapshot.data ?? []) {
      if (entry.error && !unique.has(entry.error.message)) {
        unique.set(entry.error.message, entry.error)
      }
    }
    return [...unique.values()]
  }, [resourceSnapshot.data])

  const controlVersions = useMemo(() => {
    const result = new Map<string, number>()
    for (const item of byResource.get('control-versions') ?? []) {
      const kind = String(item.fields.resource_kind ?? '')
      const id = String(item.fields.resource_id ?? '')
      if (kind && id) result.set(`${kind}:${id}`, item.version)
    }
    return result
  }, [byResource])

  const command = useMutation({
    mutationFn: async (draft: WorkforceCommandDraft) => {
      if (!session.data) throw new Error('Owner session is not ready')
      const signed = await signWorkforceCommand(session.data, draft)
      return submitWorkforceCommand(signed)
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['workforce'] })
    },
  })

  const activation = useMutation({
    mutationFn: async (name: string) => {
      if (!session.data) throw new Error('Owner session is not ready')
      return activateSignedWorkforce(session.data, name)
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['workforce'] })
    },
  })

  const workOrder = useMutation({
    mutationFn: async (order: WorkforceWorkOrder) => {
      return submitPreparedWorkOrder(order)
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['workforce'] })
    },
  })

  return {
    session: session.data ?? null,
    items: (resource: WorkforceResource) => byResource.get(resource) ?? [],
    controlVersion: (kind: string, id: string) => controlVersions.get(`${kind}:${id}`) ?? 0,
    loading: session.isLoading || resourceSnapshot.isLoading,
    error:
      [session.error, resourceSnapshot.error, ...resourceErrors]
        .filter((error): error is Error => error instanceof Error)
        .map((error) => error.message)
        .join('; ') || null,
    partial: resourceErrors.length > 0 || resourceSnapshot.isError,
    streamState,
    command,
    activation,
    workOrder,
  }
}
