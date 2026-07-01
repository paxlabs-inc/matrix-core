'use client'

/**
 * useSettings / useDaemonHealth — thin TanStack Query wrappers over the
 * daemon's GET /settings and GET /healthz. They share cache keys with
 * useDashboardData, so mounting them in the Models/Settings views reuses the
 * already-fetched data instead of issuing duplicate requests.
 */
import { useQuery } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import { getSettings, getDaemonHealth } from '@/lib/api/identity'

export function useSettings() {
  return useQuery({
    queryKey: qk.settings(),
    queryFn: ({ signal }) => getSettings(signal),
    staleTime: 60_000,
  })
}

export function useDaemonHealth() {
  return useQuery({
    queryKey: qk.health(),
    queryFn: ({ signal }) => getDaemonHealth(signal),
    staleTime: 30_000,
    refetchInterval: 60_000,
  })
}
