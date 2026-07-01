/**
 * Snapshots — wraps the daemon's MinIO-backed snapshot manager. Every
 * per-user box ships its `/data/.matrix` to the configured S3 bucket
 * on demand so a Fly Machine can be reaped + reborn without losing
 * cortex state.
 */
import { apiFetch, apiSend } from '@/lib/api/client'
import type { SnapshotsResponse } from '@/lib/api/types'

export interface SnapshotPushResult {
  status: 'pushed'
  key: string
  at: string
}

export async function getSnapshots(signal?: AbortSignal): Promise<SnapshotsResponse> {
  return apiFetch<SnapshotsResponse>('/snapshots', { signal })
}

export async function pushSnapshot(): Promise<SnapshotPushResult> {
  return apiSend<SnapshotPushResult>('/snapshots/push', undefined, { timeoutMs: 5 * 60_000 })
}
