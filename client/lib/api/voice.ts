import { apiFetch, apiSend } from '@/lib/api/client'

export interface VoiceToken {
  server_url: string
  token: string
  room: string
  expires_at: string
}

export function getVoiceToken(
  conversationId: string,
  settings: { voice: string; style: string },
): Promise<VoiceToken> {
  const query = new URLSearchParams({
    conversation_id: conversationId,
    voice: settings.voice,
    style: settings.style,
  })
  return apiFetch<VoiceToken>(`/voice/token?${query.toString()}`, { retries: 0 })
}

export function stopVoiceSession(conversationId: string): Promise<unknown> {
  return apiSend('/voice/session/stop', { conversation_id: conversationId })
}
