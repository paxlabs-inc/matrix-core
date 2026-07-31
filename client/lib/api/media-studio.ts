import { apiFetch } from '@/lib/api/client'

export type StudioKind =
  | 'text-to-image'
  | 'image-to-image'
  | 'text-to-video'
  | 'image-to-video'
  | 'inpainting'
  | 'cleanup'
  | 'remove-background'
  | 'replace-background'
  | 'remove-text'
  | 'merge-face'
  | 'upscale'

export type StudioJobStatus = 'queued' | 'running' | 'succeeded' | 'failed'

export interface StudioProviderStatus {
  configured: boolean
  provider: string
  message: string
}

export interface StudioRequest {
  kind: StudioKind
  prompt?: string
  negative_prompt?: string
  aspect_ratio?: string
  seed: number
  strength?: number
  frames?: number
  fps?: number
  scale?: number
  image?: string
  mask?: string
  face?: string
}

export interface StudioAsset {
  id: string
  media_type: 'image' | 'video'
  mime_type: string
  name: string
  size: number
  url: string
}

export interface StudioJob {
  id: string
  kind: StudioKind
  status: StudioJobStatus
  provider: 'matrix'
  progress: number
  error?: string
  request: StudioRequest
  assets: StudioAsset[]
  created_at: string
  updated_at: string
  completed_at?: string
}

type StudioJobPayload = Omit<StudioJob, 'assets'> & {
  assets?: StudioAsset[] | null
}

export function normalizeStudioJob(job: StudioJobPayload): StudioJob {
  return {
    ...job,
    assets: Array.isArray(job.assets) ? job.assets : [],
  }
}

export function getStudioStatus(signal?: AbortSignal): Promise<StudioProviderStatus> {
  return apiFetch<StudioProviderStatus>('/studio/media/status', { signal })
}

export async function listStudioJobs(signal?: AbortSignal): Promise<StudioJob[]> {
  const jobs = await apiFetch<StudioJobPayload[]>('/studio/media/jobs', { signal })
  return Array.isArray(jobs) ? jobs.map(normalizeStudioJob) : []
}

export async function createStudioJob(request: StudioRequest): Promise<StudioJob> {
  const job = await apiFetch<StudioJobPayload>('/studio/media/jobs', {
    method: 'POST',
    headers: { 'X-Matrix-Idempotency-Key': crypto.randomUUID() },
    body: JSON.stringify(request),
    retries: 0,
    timeoutMs: 30_000,
  })
  return normalizeStudioJob(job)
}

export function deleteStudioJob(jobID: string): Promise<void> {
  return apiFetch<void>(`/studio/media/jobs/${encodeURIComponent(jobID)}`, {
    method: 'DELETE',
    retries: 0,
  })
}
