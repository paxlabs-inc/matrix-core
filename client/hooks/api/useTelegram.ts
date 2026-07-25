'use client'

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  connectTelegram,
  disconnectTelegram,
  getTelegramStatus,
  type TelegramStatus,
} from '@/lib/api/telegram'

export function useTelegramStatus() {
  return useQuery<TelegramStatus | null>({
    queryKey: qk.telegram(),
    queryFn: ({ signal }) => getTelegramStatus(signal),
    staleTime: 15_000,
    refetchInterval: (query) =>
      query.state.data?.configured && !query.state.data.paired ? 5_000 : false,
    refetchIntervalInBackground: false,
  })
}

export function useConnectTelegram() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (botToken: string) => connectTelegram(botToken),
    onSuccess: (status) => queryClient.setQueryData(qk.telegram(), status),
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.telegram() }),
  })
}

export function useDisconnectTelegram() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => disconnectTelegram(),
    onSuccess: (status) => queryClient.setQueryData(qk.telegram(), status),
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.telegram() }),
  })
}
