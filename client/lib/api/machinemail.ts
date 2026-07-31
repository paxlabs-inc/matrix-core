import { ApiError, apiFetch, apiSend } from '@/lib/api/client'

export interface MachineMailStatus {
  available: boolean
  configured: boolean
  unavailable_reason?: string
}

export async function getMachineMailStatus(
  signal?: AbortSignal,
): Promise<MachineMailStatus | null> {
  try {
    return await apiFetch<MachineMailStatus>('/integrations/machinemail', { signal })
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) return null
    throw error
  }
}

export async function connectMachineMail(apiKey: string): Promise<MachineMailStatus> {
  return apiSend<MachineMailStatus>(
    '/integrations/machinemail',
    { api_key: apiKey },
    { method: 'PUT' },
  )
}

export async function disconnectMachineMail(): Promise<MachineMailStatus> {
  return apiSend<MachineMailStatus>('/integrations/machinemail', undefined, { method: 'DELETE' })
}
