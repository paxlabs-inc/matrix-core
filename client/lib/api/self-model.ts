/**
 * Self-model resource — the agent's structural self-knowledge and
 * accumulated failure patterns.
 *
 * GET /diag/self-model returns the agent's identity, structural summary
 * (from the code graph), scope, context limit, and self-authored
 * failure-pattern beliefs from the death journal consolidation.
 */
import { apiFetch } from '@/lib/api/client'

export interface FailurePattern {
  statement: string
  uri?: string
}

export interface SelfModel {
  identity: string
  resident_summary: string
  scope?: string[]
  context_limit?: number
  structural_uri?: string
  failure_patterns: FailurePattern[]
}

/** Fetch the agent's structural self-model. Returns null when the
 *  endpoint is unavailable (older daemons). */
export async function getSelfModel(signal?: AbortSignal): Promise<SelfModel | null> {
  try {
    return await apiFetch<SelfModel>('/diag/self-model', { signal })
  } catch {
    return null
  }
}
