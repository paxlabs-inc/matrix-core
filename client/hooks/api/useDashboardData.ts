'use client'

/**
 * useDashboardData — the bundled data hook the main Dashboard component
 * subscribes to. Pulls runs + activity + stats + agents + tools + me
 * via parallel queries and surfaces a single object with sensible
 * fallbacks so the dashboard renders something useful in every state
 * (cold-start, reconnect, offline).
 *
 * The hook is deliberately backend-agnostic: when the router URL is
 * unreachable or the user is not yet signed in, every field falls back
 * to the static UI fixtures from `lib/matrix-data` so the layout stays
 * intact. This keeps the visual contract identical to the mock-only
 * baseline that ships today.
 */
import { useQuery } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import { getDashboardSnapshot } from '@/lib/api/runs'
import { getAgentRoster, getToolCatalog } from '@/lib/api/agents'
import { getMe, getSettings } from '@/lib/api/identity'
import { settingsToActiveModelId } from '@/lib/api/models'
import { pickDefaultSkillUri } from '@/lib/api/mappers'
import {
  agents as fallbackAgents,
  tools as fallbackTools,
  activity as fallbackActivity,
  stats as fallbackStats,
  type Run,
  type Agent,
  type Tool,
  type ActivityDay,
  type Stats,
} from '@/lib/matrix-data'

export interface DashboardData {
  runs: Run[]
  stats: Stats
  activity: ActivityDay[]
  agents: Agent[]
  tools: Tool[]
  /** Current user display label (email/handle/did fallback chain). */
  userLabel: string
  /** Active model id matched against `models[]` in matrix-data, or null. */
  activeModelId: string | null
  /** Skill URI to dispatch against by default — the daemon's configured
   *  skill, else the first live skill in the roster. Null when no skill
   *  catalogue is reachable (offline). */
  activeSkillUri: string | null
  loading: boolean
  /** Aggregated error summary; null when every query is OK or fell back. */
  error: string | null
  /** True when the dashboard is rendering live data rather than mocks. */
  live: boolean
}

const DEFAULT_USER_LABEL = 'Operator'

export function useDashboardData(): DashboardData {
  const dash = useQuery({
    queryKey: qk.dashboardSnapshot(),
    queryFn: ({ signal }) => getDashboardSnapshot(signal),
    refetchInterval: 15_000,
    // Don't burn battery/bandwidth polling a tab the user can't see; the
    // query refetches on window focus + reconnect, so it catches up the
    // moment they come back.
    refetchIntervalInBackground: false,
  })
  const ag = useQuery({
    queryKey: qk.agentRoster(),
    queryFn: ({ signal }) => getAgentRoster(signal),
    staleTime: 5 * 60_000,
  })
  const tl = useQuery({
    queryKey: qk.toolsCatalog(),
    queryFn: ({ signal }) => getToolCatalog(signal),
    staleTime: 5 * 60_000,
  })
  const me = useQuery({
    queryKey: qk.me(),
    queryFn: ({ signal }) => getMe(signal),
    staleTime: 60_000,
  })
  const s = useQuery({
    queryKey: qk.settings(),
    queryFn: ({ signal }) => getSettings(signal),
    staleTime: 60_000,
  })

  const live = dash.isSuccess
  const errorMsg =
    [dash.error, ag.error, tl.error, me.error, s.error]
      .map((e) => (e instanceof Error ? e.message : null))
      .filter(Boolean)
      .join('; ') || null

  return {
    runs: dash.data?.runs ?? [],
    stats: dash.data?.stats ?? fallbackStats,
    activity: dash.data?.activity ?? fallbackActivity,
    agents: ag.data ?? fallbackAgents,
    tools: tl.data ?? fallbackTools,
    userLabel: deriveUserLabel(me.data, DEFAULT_USER_LABEL),
    activeModelId: settingsToActiveModelId(s.data ?? null),
    activeSkillUri: pickDefaultSkillUri(s.data?.skill ?? me.data?.skill, ag.data),
    loading: dash.isLoading || ag.isLoading || tl.isLoading,
    error: errorMsg,
    live,
  }
}

function deriveUserLabel(
  me: { user_id?: string; user_uri?: string; agent?: string } | undefined,
  fallback: string,
): string {
  if (!me) return fallback
  // Prefer a friendly handle if the daemon ever attaches one; for now
  // surface the agent role or the trimmed user id.
  if (me.agent) return capitalize(me.agent)
  const id = me.user_id ?? me.user_uri ?? ''
  if (!id) return fallback
  // UUIDs are noisy in headers; show the leading 8 chars.
  return id.length > 12 ? `User ${id.slice(0, 8)}` : id
}

function capitalize(s: string): string {
  return s.length > 0 ? s[0].toUpperCase() + s.slice(1) : s
}
