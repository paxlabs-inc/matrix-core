/**
 * Morning brief + personalization resource module — mirrors the daemon's
 * authenticated /brief/* + /interview/* control surfaces
 * (neo/internal/server/brief_routes.go, interview_routes.go) and the
 * per-user daemon's /personalization profile routes.
 *
 * The morning brief is Neo's opt-in personalized daily digest: a short,
 * source-backed brief composed while the user is away and delivered as a
 * normal conversation turn + inbox item at their chosen local time. This
 * module exposes the schedule (enable/pause, time, timezone, days, length,
 * sections), the guided-interview entry, the saved profile
 * (view/export/delete), and explicit item feedback. Copy carries the RESULT,
 * never the protocol — no alarm/marker/cortex jargon leaks through.
 *
 * getBriefSettings returns null when the daemon reports the control is
 * unavailable (503/404) — older daemons without the feature wired — so the UI
 * hides the section rather than surfacing an error.
 */
import { apiFetch, apiSend, ApiError } from '@/lib/api/client'

/** GET/PUT /brief/settings — the schedule + opt-in state. */
export interface BriefSettings {
  enabled: boolean
  paused: boolean
  alarm_live: boolean
  timezone?: string
  delivery_time?: string
  days?: number[]
  length?: string
  sections?: string[]
}

/** PUT /brief/settings — every field optional (only what changed). */
export interface BriefSettingsUpdate {
  enabled?: boolean
  paused?: boolean
  timezone?: string
  delivery_time?: string
  days?: number[]
  length?: string
  sections?: string[]
  conversation_id?: string
}

/** The closed section vocabulary (matches the server + profile schema). */
export const BRIEF_SECTIONS = [
  'news',
  'music',
  'movies_tv',
  'books',
  'games',
  'daily_assistance',
] as const

/** The closed length vocabulary. */
export const BRIEF_LENGTHS = ['short', 'standard', 'deep'] as const

/** The saved personalization profile (the daemon's inspectable record). */
export interface PersonalizationProfile {
  schema_version: number
  interests?: string[]
  day_to_day_goals?: string[]
  media?: Record<string, { liked?: string[]; disliked?: string[] }>
  adventurousness?: string
  brief_preferences?: { length?: string; sections?: string[] }
}

export interface PersonalizationResponse {
  profile: PersonalizationProfile
  uri?: string
}

/** The schedule, or null when the daemon has no brief control wired. */
export async function getBriefSettings(signal?: AbortSignal): Promise<BriefSettings | null> {
  try {
    return await apiFetch<BriefSettings>('/brief/settings', { signal })
  } catch (e) {
    if (e instanceof ApiError && (e.status === 503 || e.status === 404)) return null
    throw e
  }
}

/** Apply a partial schedule/opt-in update. */
export async function putBriefSettings(update: BriefSettingsUpdate): Promise<BriefSettings> {
  return apiSend<BriefSettings>('/brief/settings', update, { method: 'PUT' })
}

/** Record an explicit reaction to a delivered brief item. */
export async function sendBriefFeedback(item: string, verdict: string): Promise<void> {
  await apiSend('/brief/feedback', { item, verdict }, { method: 'POST' })
}

/** Start (or repeat) the guided interview: mints the interview conversation. */
export async function startInterview(): Promise<{ conversation_id: string }> {
  return apiSend<{ conversation_id: string }>('/interview/start', {}, { method: 'POST' })
}

/** The saved profile, or null when personalization isn't available. */
export async function getPersonalization(
  signal?: AbortSignal,
): Promise<PersonalizationResponse | null> {
  try {
    return await apiFetch<PersonalizationResponse>('/personalization', { signal })
  } catch (e) {
    if (e instanceof ApiError && (e.status === 503 || e.status === 404)) return null
    throw e
  }
}

/** The profile with its version lineage, for user data export (download). */
export async function exportPersonalization(signal?: AbortSignal): Promise<unknown> {
  return apiFetch<unknown>('/personalization/export', { signal })
}

/** Delete the saved profile (the schedule survives separately). */
export async function deletePersonalization(): Promise<{ deleted: boolean }> {
  return apiSend<{ deleted: boolean }>('/personalization', undefined, { method: 'DELETE' })
}
