/**
 * Agents + tools + skills resource module.
 *
 * The daemon exposes catalogue endpoints that the dispatch dialog uses
 * to populate "which agent / which tools" choices. The mappers fall
 * back to the static UI fixtures when the daemon catalogue is empty
 * (offline / dev mode), so the UI never renders an unusable empty
 * picker.
 */
import { apiFetch } from '@/lib/api/client'
import type { ToolEntry, SkillEntry } from '@/lib/api/types'
import type { Agent, Tool } from '@/lib/matrix-data'
import { agents as fallbackAgents, tools as fallbackTools } from '@/lib/matrix-data'
import { skillsToAgents, toolEntriesToTools } from '@/lib/api/mappers'

export async function getTools(signal?: AbortSignal): Promise<ToolEntry[] | null> {
  try {
    const r = await apiFetch<{ items?: ToolEntry[] } | ToolEntry[]>('/tools', { signal })
    if (Array.isArray(r)) return r
    return r.items ?? null
  } catch {
    return null
  }
}

export async function getSkills(signal?: AbortSignal): Promise<SkillEntry[] | null> {
  try {
    const r = await apiFetch<{ items?: SkillEntry[] } | SkillEntry[]>('/skills', { signal })
    if (Array.isArray(r)) return r
    return r.items ?? null
  } catch {
    return null
  }
}

/** Roster the dispatch dialog renders. Built from the daemon's live
 *  skill catalogue (each skill is a capability the agent can run), with
 *  a graceful fall back to the static roster when offline. */
export async function getAgentRoster(signal?: AbortSignal): Promise<Agent[]> {
  const skills = await getSkills(signal)
  return skillsToAgents(skills) ?? fallbackAgents
}

/** Tool list the dispatch dialog renders. Live → mapped → UI fallback. */
export async function getToolCatalog(signal?: AbortSignal): Promise<Tool[]> {
  const live = await getTools(signal)
  return toolEntriesToTools(live) ?? fallbackTools
}
