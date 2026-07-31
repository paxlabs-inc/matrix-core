'use client'

/**
 * Onboarding + beta-launch hooks. Wraps the API functions in
 * lib/api/onboarding.ts with TanStack Query for the onboarding flow:
 * invite redemption, consent, disclosure ack, provisioning status polling,
 * and profile read/write.
 */
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  startOnboarding,
  getConsent,
  putConsent,
  ackDisclosure,
  getProvisionStatus,
  getProfile,
  putProfile,
  getSubscription,
  claimLaunchSubscription,
  type ProfileRequest,
} from '@/lib/api/onboarding'

const POLL_INTERVAL = 3000

export function useStartOnboarding() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => startOnboarding(),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['provision-status'] })
    },
  })
}

export function useSubscription() {
  return useQuery({
    queryKey: ['subscription'],
    queryFn: ({ signal }) => getSubscription(signal),
    staleTime: 30_000,
  })
}

export function useClaimLaunchSubscription() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => claimLaunchSubscription(),
    onSuccess: (subscription) => {
      qc.setQueryData(['subscription'], subscription)
    },
  })
}

export function useConsent() {
  return useQuery({
    queryKey: ['consent'],
    queryFn: ({ signal }) => getConsent(signal),
    staleTime: 30_000,
  })
}

export function usePutConsent() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ optIn, policyVersion }: { optIn: boolean; policyVersion: string }) =>
      putConsent(optIn, policyVersion),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['consent'] })
    },
  })
}

export function useAckDisclosure() {
  return useMutation({
    mutationFn: (version: string) => ackDisclosure(version),
  })
}

export function useProvisionStatus(enabled: boolean) {
  return useQuery({
    queryKey: ['provision-status'],
    queryFn: ({ signal }) => getProvisionStatus(signal),
    enabled,
    retry: false,
    refetchOnWindowFocus: false,
    staleTime: 0,
    refetchInterval: (query) => {
      const state = query.state.data?.state
      if (state === 'active' || state === 'failed') return false
      return POLL_INTERVAL
    },
  })
}

export function useProfile() {
  return useQuery({
    queryKey: ['profile'],
    queryFn: ({ signal }) => getProfile(signal),
    retry: 1,
    staleTime: 60_000,
  })
}

export function usePutProfile() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (req: ProfileRequest) => putProfile(req),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['profile'] })
    },
  })
}
