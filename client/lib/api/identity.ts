/**
 * Identity + settings — mirrors GET /me, /settings on the daemon.
 * Used by the model picker and the wallet header to surface the
 * current actor without leaking router/gateway plumbing into the UI.
 */
import { apiFetch } from '@/lib/api/client'
import type { MeResponse, SettingsResponse, HealthzResponse } from '@/lib/api/types'

export async function getMe(signal?: AbortSignal): Promise<MeResponse> {
  return apiFetch<MeResponse>('/me', { signal })
}

export async function getSettings(signal?: AbortSignal): Promise<SettingsResponse> {
  return apiFetch<SettingsResponse>('/settings', { signal })
}

export async function getDaemonHealth(signal?: AbortSignal): Promise<HealthzResponse> {
  return apiFetch<HealthzResponse>('/healthz', { signal })
}
