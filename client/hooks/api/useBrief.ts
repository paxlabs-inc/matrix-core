'use client'

/**
 * Morning-brief + personalization hooks — the schedule (enable/pause, time,
 * timezone, days, length, sections), the guided-interview entry, and the saved
 * profile (view/export/delete), over the daemon's /brief/*, /interview/*, and
 * /personalization surfaces.
 *
 * useBriefSettings resolves to `null` when the daemon has no brief control
 * wired, letting the Settings UI hide the section entirely. Updates are
 * optimistic (the switch/fields apply instantly, roll back on error).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  deletePersonalization,
  exportPersonalization,
  getBriefSettings,
  getPersonalization,
  putBriefSettings,
  startInterview,
  type BriefSettings,
  type BriefSettingsUpdate,
  type PersonalizationResponse,
} from '@/lib/api/brief'

/* ── Queries ─────────────────────────────────────────────────────────────── */

/** The schedule + opt-in state, or null when unavailable. */
export function useBriefSettings() {
  return useQuery<BriefSettings | null>({
    queryKey: qk.briefSettings(),
    queryFn: ({ signal }) => getBriefSettings(signal),
    staleTime: 30_000,
  })
}

/** The saved personalization profile, or null when unavailable. Only fetched
 *  when `enabled` (the profile dialog is open) to avoid a wasted call. */
export function usePersonalization(enabled = true) {
  return useQuery<PersonalizationResponse | null>({
    queryKey: qk.personalization(),
    queryFn: ({ signal }) => getPersonalization(signal),
    enabled,
    staleTime: 15_000,
  })
}

/* ── Mutations ───────────────────────────────────────────────────────────── */

/** Apply a partial schedule update, optimistically. */
export function useUpdateBriefSettings() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (update: BriefSettingsUpdate) => putBriefSettings(update),
    onMutate: async (update) => {
      await qc.cancelQueries({ queryKey: qk.briefSettings() })
      const prev = qc.getQueryData<BriefSettings | null>(qk.briefSettings())
      if (prev) qc.setQueryData(qk.briefSettings(), { ...prev, ...update })
      return { prev }
    },
    onError: (_e, _v, ctx) => {
      if (ctx?.prev !== undefined) qc.setQueryData(qk.briefSettings(), ctx.prev)
    },
    onSuccess: (fresh) => qc.setQueryData(qk.briefSettings(), fresh),
  })
}

/** Start (or repeat) the guided interview; resolves the conversation id. */
export function useStartInterview() {
  return useMutation({ mutationFn: () => startInterview() })
}

/** Delete the saved profile (the schedule survives separately). */
export function useDeletePersonalization() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => deletePersonalization(),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.personalization() }),
  })
}

/** Download the profile export as a JSON file. */
export function useExportPersonalization() {
  return useMutation({
    mutationFn: async () => {
      const data = await exportPersonalization()
      const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = 'personalization-profile.json'
      a.click()
      URL.revokeObjectURL(url)
    },
  })
}
