/**
 * Mappers — translate backend wire types into the UI types declared
 * in `lib/matrix-data.ts`. Keeping the boundary explicit means UI
 * components don't have to reason about platform-specific concepts
 * (envelopes, replay roots, lifecycle paths) and the wire shape can
 * evolve without rippling through the dashboard.
 */
import type {
  Run,
  RunStatus,
  RunStage,
  StageStatus,
  Receipt,
  ReceiptArtifact,
  Agent,
  Tool,
  Stats,
  ActivityDay,
  ActivityItem,
  ActivityKind,
} from '@/lib/matrix-data'
import { buildStages } from '@/lib/matrix-data'
import type {
  IntentSummary,
  IntentAttestation,
  ToolEntry,
  SkillEntry,
  SSEEvent,
} from '@/lib/api/types'

/** Map daemon intent state → UI run status. */
export function mapRunStatus(
  s: IntentSummary['state'],
  async?: IntentSummary['async_status'],
): RunStatus {
  // Async-mode wins when present so we never show "executing" for a
  // job that has been cancelled out of band.
  if (async) {
    switch (async) {
      case 'queued':
        return 'queued'
      case 'running':
        return 'running'
      case 'completed':
        return 'completed'
      case 'failed':
        return 'failed'
      case 'cancelled':
        return 'failed'
    }
  }
  switch (s) {
    case 'drafting':
    case 'proposed':
    case 'clarifying':
      return 'queued'
    case 'accepted':
      return 'running'
    case 'executing':
      return 'running'
    case 'completed':
      return 'completed'
    case 'failed':
      return 'failed'
    case 'cancelled':
      return 'failed'
    default:
      return 'running'
  }
}

/** Convert an intent state to the index of the active pipeline stage
 *  (Plan → Gather → Execute → Verify → Sign). */
export function stageIndex(state: IntentSummary['state']): { active: number; failedAt?: number } {
  switch (state) {
    case 'drafting':
      return { active: 0 }
    case 'proposed':
    case 'clarifying':
      return { active: 1 }
    case 'accepted':
      return { active: 2 }
    case 'executing':
      return { active: 2 }
    case 'completed':
      return { active: 5 }
    case 'failed':
      // Best-effort: we don't know which phase failed without parsing
      // the lifecycle; default to "execute" as the most common.
      return { active: 2, failedAt: 2 }
    case 'cancelled':
      return { active: 2, failedAt: 2 }
    default:
      return { active: 0 }
  }
}

/**
 * Derive a live, SSE-driven stage array from the events stream.
 *
 * `seedStages` is the snapshot mapped from the polled IntentSummary —
 * it carries the right baseline when a transcript is opened mid-run
 * before any events have arrived. As events arrive, we refine the
 * stage status per the protocol's lifecycle.transition envelopes plus
 * a few well-known walker/envelope events so the Plan → Gather →
 * Execute → Verify → Sign stepper actually advances live instead of
 * snapping at the next dashboard refetch.
 *
 * Mapping (closed taxonomy from daemon's lifecycle driver):
 *   to=proposed   → Plan done, Gather active            (active >= 1)
 *   to=accepted   → Plan/Gather done, Execute active    (active >= 2)
 *   to=executing  → Execute active                      (active >= 2)
 *   walk.cortex.post → Execute done, Verify active      (active >= 3)
 *   envelope.signed kind=intent.attest → Sign active    (active >= 4)
 *   to=completed | message.complete status=completed → all done
 *   to=failed | to=cancelled | message.complete status=failed → mark failedAt
 *
 * Conservative: if no recognised events have been seen we return the
 * seed unchanged. The function never *regresses* a stage — once Verify
 * is active we never drop back to Execute.
 */
export function deriveLiveStages(
  seedStages: RunStage[],
  events: SSEEvent[],
): { stages: RunStage[]; allDone: boolean; failedAt?: number } {
  if (events.length === 0) return { stages: seedStages, allDone: false }

  // Seed `active` from the snapshot so an event-stream that opens
  // mid-run (only carrying late-stage events) doesn't accidentally
  // walk the stepper backwards. We pick the largest done-or-active
  // index from the seed.
  let active = -1
  for (let i = 0; i < seedStages.length; i++) {
    const st = seedStages[i].status
    if (st === 'done' || st === 'active') active = Math.max(active, i)
  }
  let failedAt: number | undefined
  let allDone = false

  for (const ev of events) {
    if (ev.type === 'lifecycle.transition') {
      const to = typeof ev.fields?.to === 'string' ? (ev.fields.to as string) : ''
      switch (to) {
        case 'proposed':
          active = Math.max(active, 1)
          break
        case 'clarifying':
          active = Math.max(active, 1)
          break
        case 'accepted':
          active = Math.max(active, 2)
          break
        case 'executing':
          active = Math.max(active, 2)
          break
        case 'completed':
          allDone = true
          active = 4
          break
        case 'failed':
        case 'cancelled':
          failedAt = active < 0 ? 2 : Math.max(active, 2)
          break
      }
    }
    if (ev.type === 'walk.cortex.post') {
      active = Math.max(active, 3)
    }
    if (ev.type === 'envelope.signed') {
      const kind = typeof ev.fields?.kind === 'string' ? (ev.fields.kind as string) : ''
      if (kind === 'intent.attest') {
        active = Math.max(active, 4)
      } else if (kind === 'intent.fail' || kind === 'intent.cancel') {
        failedAt = active < 0 ? 2 : Math.max(active, 2)
      }
    }
    if (ev.type === 'message.complete') {
      const status = typeof ev.fields?.status === 'string' ? (ev.fields.status as string) : ''
      if (status === 'completed') {
        allDone = true
        active = 4
      } else if (status === 'failed') {
        failedAt = active < 0 ? 2 : Math.max(active, 2)
      }
    }
  }

  if (allDone) {
    return {
      stages: seedStages.map((s) => ({ ...s, status: 'done' as StageStatus })),
      allDone,
    }
  }
  if (failedAt !== undefined) {
    return { stages: buildStages(Math.max(active, 0), failedAt), allDone: false, failedAt }
  }
  if (active < 0) return { stages: seedStages, allDone: false }
  return { stages: buildStages(active), allDone: false }
}

/** Best-effort progress percentage (0..100). The daemon doesn't ship
 *  a numeric progress today; we synthesise from the stage index. */
export function deriveProgress(state: IntentSummary['state'], hasAttest: boolean): number {
  if (hasAttest || state === 'completed') return 100
  const map: Record<IntentSummary['state'], number> = {
    drafting: 6,
    proposed: 20,
    clarifying: 25,
    accepted: 45,
    executing: 70,
    completed: 100,
    failed: 100,
    cancelled: 100,
  }
  return map[state] ?? 30
}

const DEFAULT_AGENT_ID = 'agt_research'

/** Map a wire intentSummary into a UI Run. The mapping is lossy by
 *  design — the UI cares about state, label, progress, and counts; the
 *  rich envelope chain is fetched on-demand for receipt detail. */
export function intentToRun(s: IntentSummary): Run {
  const { active, failedAt } = stageIndex(s.state)
  const status = mapRunStatus(s.state, s.async_status)
  const stages: RunStage[] = buildStages(active, failedAt)
  // Mark the post-execute stages as done if a signed attestation lives
  // on disk — this gives the user honest "verified" + "signed" ticks.
  if (s.has_attest) {
    for (let i = 0; i < stages.length; i++) {
      stages[i] = { ...stages[i], status: 'done' as StageStatus }
    }
  }
  return {
    id: s.intent_id,
    title: s.prose ?? s.verb ?? s.intent_id,
    status,
    agentId: deriveAgentId(s),
    toolIds: deriveToolIds(s),
    autonomy: 'checkpoints',
    progress: deriveProgress(s.state, s.has_attest),
    stages,
    createdAt: s.started_at ?? new Date().toISOString(),
    completedAt: s.ended_at,
    durationMs: s.duration_ms,
    receiptId: s.has_attest ? `rcpt_${s.intent_id}` : undefined,
  }
}

function deriveAgentId(s: IntentSummary): string {
  // Heuristic: skill slug "X-..." → agent "agt_X" if the dashboard has
  // it, else fall back to the research agent.
  const slug = (s.skill ?? '').match(/skill\/([^@]+)/)?.[1] ?? ''
  if (slug.startsWith('paxeer-')) return 'agt_ops'
  if (slug.startsWith('forge-')) return 'agt_builder'
  if (slug.startsWith('inbox') || slug.includes('mail')) return 'agt_inbox'
  return DEFAULT_AGENT_ID
}

function deriveToolIds(_s: IntentSummary): string[] {
  // Not derivable from the summary alone — the per-intent plan call
  // gives us the actual tool list. Default to the universally-enabled
  // tool set so the run card stays informative.
  return ['tool_web', 'tool_files']
}

/** Map a parsed intent.attest body into a UI Receipt. */
export function attestationToReceipt(
  intentId: string,
  prose: string,
  att: IntentAttestation,
  fallbackEndedAt?: string,
): Receipt {
  const artifacts: ReceiptArtifact[] = (att.artifacts ?? []).map((a, i) => ({
    id: a.id ?? `a${i}`,
    name: a.name ?? a.uri ?? `artifact-${i + 1}`,
    kind: normaliseArtifactKind(a.kind),
    summary: a.summary ?? '',
  }))
  const replayMatch = Boolean(
    att.pre_replay_root && att.post_replay_root && att.pre_replay_root === att.post_replay_root,
  )
  return {
    id: `rcpt_${intentId}`,
    runId: intentId,
    title: prose,
    signedAt: att.signed_at ?? fallbackEndedAt ?? new Date().toISOString(),
    hash: att.intent_hash ?? '',
    signature: att.signature ?? '',
    replayVerified: replayMatch,
    stepCount: att.step_count ?? 0,
    toolCallCount: att.tool_call_count ?? 0,
    costUsd: att.cost_usd ?? 0,
    artifacts,
  }
}

function normaliseArtifactKind(k?: string): ReceiptArtifact['kind'] {
  switch (k) {
    case 'file':
    case 'message':
    case 'commit':
    case 'record':
    case 'link':
      return k
    default:
      return 'record'
  }
}

/** Extract the slug from a skill URI (matrix://skill/<slug>@<v>). */
export function skillSlug(skillUri: string | undefined): string {
  if (!skillUri) return ''
  const m = skillUri.match(/skill\/([^@/]+)/)
  if (m) return m[1]
  // Bare slug or unexpected shape — strip any version suffix.
  return skillUri.split('/').pop()?.split('@')[0] ?? ''
}

/** Humanise a skill slug into a display name, e.g. "paxeer-token-safety"
 *  → "Token Safety". */
export function prettifySlug(slug: string): string {
  const cleaned = slug
    .replace(/^paxeer[-_]?/i, '')
    .replace(/[-_]+/g, ' ')
    .trim()
  const base = cleaned || slug
  return base.replace(/\b\w/g, (c) => c.toUpperCase())
}

/** Short role label inferred from the skill slug + description, used as
 *  the persona subtitle in the agents roster. */
function roleForSkill(slug: string, description?: string): string {
  const s = slug.toLowerCase()
  const d = (description ?? '').toLowerCase()
  const hay = `${s} ${d}`
  if (/assistant|general|concierge/.test(hay)) return 'General assistant'
  if (/safety|guard|risk|audit|secur/.test(hay)) return 'Risk & safety'
  if (/defi|swap|trade|liquidit|yield|stak/.test(hay)) return 'DeFi operations'
  if (/research|analy|insight|report/.test(hay)) return 'Research & analysis'
  if (/inbox|mail|message|comm|notify/.test(hay)) return 'Inbox & comms'
  if (/build|forge|deploy|compile|code/.test(hay)) return 'Build & deploy'
  if (/wallet|pay|treasury|invoice|spend/.test(hay)) return 'Wallet & payments'
  if (/data|sheet|reconcil|ledger|ops/.test(hay)) return 'Data & ops'
  return 'Centra AI skill'
}

/** Build the user-facing agent roster from the daemon's live skill
 *  catalogue (GET /skills). Each installed skill is a capability the
 *  user's agent can be pointed at, carrying the skill URI so dispatch
 *  can compile against it. Returns null when the catalogue is empty so
 *  the caller falls back to the static roster. */
export function skillsToAgents(skills: SkillEntry[] | null | undefined): Agent[] | null {
  if (!skills || skills.length === 0) return null
  const seen = new Set<string>()
  const out: Agent[] = []
  for (const sk of skills) {
    const slug = sk.slug || skillSlug(sk.uri)
    if (!slug || seen.has(slug)) continue
    seen.add(slug)
    out.push({
      id: slug,
      name: sk.display?.trim() || prettifySlug(slug),
      role: roleForSkill(slug, sk.description),
      description: sk.description?.trim() || '',
      status: 'idle',
      runsCompleted: 0,
      successRate: 0,
      skillUri: sk.uri,
    })
  }
  return out.length > 0 ? out : null
}

/** Choose the skill URI to dispatch against by default. Prefers the
 *  daemon's configured skill, then the first roster entry that carries
 *  a live skill URI. Returns null when nothing usable is available. */
export function pickDefaultSkillUri(
  configured: string | null | undefined,
  roster: Agent[] | null | undefined,
): string | null {
  const c = (configured ?? '').trim()
  if (c.startsWith('matrix://skill/')) return c
  const fromRoster = (roster ?? []).find((a) => a.skillUri)?.skillUri
  return fromRoster ?? null
}

/** Convert /tools wire entries to UI Tool[]. */
export function toolEntriesToTools(entries: ToolEntry[] | null | undefined): Tool[] | null {
  if (!entries || entries.length === 0) return null
  return entries.map((t) => ({
    id: t.uri,
    name: t.name || lastSegment(t.uri),
    category: t.server ?? 'mcp',
    description: t.description ?? '',
    enabled: t.enabled ?? true,
  }))
}

function lastSegment(uri: string): string {
  const i = uri.lastIndexOf('/')
  return i === -1 ? uri : uri.slice(i + 1)
}

/** Aggregate a list of intents into the at-a-glance Stats card. */
export function intentsToStats(items: IntentSummary[]): Stats {
  let completed = 0
  let active = 0
  let totalMs = 0
  let totalRuns = 0
  for (const s of items) {
    totalRuns++
    if (s.state === 'completed') completed++
    if (s.state === 'executing' || s.state === 'accepted') active++
    if (s.duration_ms) totalMs += s.duration_ms
  }
  const completionRate = totalRuns === 0 ? 0 : Math.round((completed / totalRuns) * 100)
  return {
    tasksCompleted: completed,
    completionRate,
    activeNow: active,
    timeReclaimedHrs: Math.round(totalMs / 3_600_000),
  }
}

/** Build the activity timeline from a flat intent list, grouped by
 *  calendar day in the user's locale. */
export function intentsToActivity(items: IntentSummary[]): ActivityDay[] {
  const byDate = new Map<string, ActivityItem[]>()
  const fmt = new Intl.DateTimeFormat(undefined, { weekday: 'long' })
  const today = new Date()
  // `today` + `fmt` are reused below in labelForKey when emitting the
  // grouped output.
  for (const s of items) {
    const at = new Date(s.ended_at ?? s.started_at ?? Date.now())
    const key = startOfDay(at).toISOString()
    const time = at.toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })
    const text = s.prose ?? s.verb ?? s.intent_id
    const list = byDate.get(key) ?? []
    list.push({
      id: `ac_${s.intent_id}`,
      kind: kindForState(s.state),
      text,
      agent: 'Centra AI',
      time,
    })
    byDate.set(key, list)
  }
  // Stable order: newest day first; items already chronologically descending if input is.
  const keys = [...byDate.keys()].sort((a, b) => (a < b ? 1 : -1))
  return keys.map((k) => ({
    date: labelForKey(k, today, fmt),
    items: byDate.get(k)!,
  }))
}

function startOfDay(d: Date): Date {
  const c = new Date(d)
  c.setHours(0, 0, 0, 0)
  return c
}

function labelForDay(d: Date, today: Date, fmt: Intl.DateTimeFormat): string {
  const a = startOfDay(d).getTime()
  const b = startOfDay(today).getTime()
  const dayMs = 86_400_000
  if (a === b) return `Today / ${fmt.format(d)}`
  if (a === b - dayMs) return `Yesterday / ${fmt.format(d)}`
  return fmt.format(d)
}

function labelForKey(key: string, today: Date, fmt: Intl.DateTimeFormat): string {
  return labelForDay(new Date(key), today, fmt)
}

function kindForState(s: IntentSummary['state']): ActivityKind {
  switch (s) {
    case 'completed':
      return 'completed'
    case 'failed':
    case 'cancelled':
      return 'failed'
    case 'clarifying':
      return 'needs_input'
    default:
      return 'dispatched'
  }
}
