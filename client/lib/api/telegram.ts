import { ApiError, apiFetch, apiSend } from '@/lib/api/client'

export interface TelegramStatus {
  available: boolean
  configured: boolean
  connected: boolean
  paired: boolean
  bot_username?: string
  telegram_username?: string
  pairing_url?: string
  last_error?: string
  unavailable_reason?: string
}

export async function getTelegramStatus(signal?: AbortSignal): Promise<TelegramStatus | null> {
  try {
    return await apiFetch<TelegramStatus>('/integrations/telegram', { signal })
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) return null
    throw error
  }
}

export async function connectTelegram(botToken: string): Promise<TelegramStatus> {
  return apiSend<TelegramStatus>(
    '/integrations/telegram',
    { bot_token: botToken },
    { method: 'PUT' },
  )
}

export async function disconnectTelegram(): Promise<TelegramStatus> {
  return apiSend<TelegramStatus>('/integrations/telegram', undefined, { method: 'DELETE' })
}
