'use client'

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  connectMachineMail,
  disconnectMachineMail,
  getMachineMailStatus,
  type MachineMailStatus,
} from '@/lib/api/machinemail'

export function useMachineMailStatus() {
  return useQuery<MachineMailStatus | null>({
    queryKey: qk.machineMail(),
    queryFn: ({ signal }) => getMachineMailStatus(signal),
    staleTime: 15_000,
  })
}

export function useConnectMachineMail() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (apiKey: string) => connectMachineMail(apiKey),
    onSuccess: (status) => queryClient.setQueryData(qk.machineMail(), status),
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.machineMail() }),
  })
}

export function useDisconnectMachineMail() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => disconnectMachineMail(),
    onSuccess: (status) => queryClient.setQueryData(qk.machineMail(), status),
    onSettled: () => queryClient.invalidateQueries({ queryKey: qk.machineMail() }),
  })
}
