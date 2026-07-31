/**
 * Media resource — Neo's image / video / audio plane.
 *
 * Generated and uploaded media live on the agent's own machine volume and are
 * served (behind per-user auth) from `/media/<name>` by the Neo server.
 *
 *   - `uploadMedia` POSTs an attachment to `/upload`; Neo stores it and returns
 *     a `/media/<name>` reference the next chat message embeds so the model can
 *     pass it to edit_image / generate_video / transcribe_audio.
 *   - `loadMediaObjectURL` fetches a `/media/<name>` reference WITH the Supabase
 *     bearer (an <img>/<video> src cannot carry it) and returns a blob object
 *     URL for rendering. Callers must revoke the URL when done.
 */
import { apiFetch } from '@/lib/api/client'

export type MediaKind = 'image' | 'video' | 'audio' | 'file'

export interface UploadedMedia {
  /** Relative reference, e.g. "/media/2026..ab12.png". */
  url: string
  name: string
  kind: MediaKind
  mime: string
  bytes: number
}

/** Upload one attachment to the agent's machine volume. */
export async function uploadMedia(file: Blob, filename: string): Promise<UploadedMedia> {
  const form = new FormData()
  form.append('file', file, filename)
  return apiFetch<UploadedMedia>('/upload', {
    method: 'POST',
    body: form,
    // Uploads are not idempotent and can be large; no retries, generous timeout.
    retries: 0,
    timeoutMs: 120_000,
  })
}

/**
 * Upload a composer attachment. The PromptInput represents files as blob
 * object URLs (FileUIPart.url), so we re-read the bytes before uploading.
 */
export async function uploadFromObjectURL(
  objectURL: string,
  filename: string,
): Promise<UploadedMedia> {
  const res = await fetch(objectURL)
  const blob = await res.blob()
  return uploadMedia(blob, filename)
}

/** Bucket a MIME type into the kind used for the attachment marker. */
export function mediaKindForMime(mime: string): MediaKind {
  if (mime.startsWith('image/')) return 'image'
  if (mime.startsWith('video/')) return 'video'
  if (mime.startsWith('audio/')) return 'audio'
  return 'file'
}

/**
 * Fetch a `/media/<name>` reference as an authed blob and return an object URL
 * for rendering. Returns null on failure. Caller owns revoking the URL.
 */
export async function loadMediaObjectURL(ref: string): Promise<string | null> {
  if (!ref) return null
  try {
    const res = await apiFetch<Response>(ref, { raw: true, timeoutMs: 60_000 })
    const blob = await res.blob()
    return URL.createObjectURL(blob)
  } catch {
    return null
  }
}

/**
 * Download a `/media/<name>` (or other same-origin daemon) reference to the
 * user's device, carrying the Supabase bearer (an <a download> cannot). Fetches
 * the authed blob, then triggers a transient object-URL download. Returns false
 * on failure so the caller can surface a toast.
 */
export async function downloadMediaRef(ref: string, filename?: string): Promise<boolean> {
  if (!ref) return false
  try {
    const res = await apiFetch<Response>(ref, { raw: true, timeoutMs: 120_000 })
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename || ref.split('/').pop() || 'download'
    document.body.appendChild(a)
    a.click()
    a.remove()
    // Revoke on the next tick so the click has flushed.
    setTimeout(() => URL.revokeObjectURL(url), 0)
    return true
  } catch {
    return false
  }
}
