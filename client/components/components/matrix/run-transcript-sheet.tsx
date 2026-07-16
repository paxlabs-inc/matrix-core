'use client'

/**
 * RunTranscriptSheet — the live, connected window into a single run.
 *
 * Opens from a run's "Open transcript" action and subscribes to the
 * daemon's Server-Sent-Events stream (GET /events?intent_id=…) through
 * the resilient SSE client (see lib/realtime/sse.ts → useRunEvents).
 * Every plan/tool/verify/sign step the agent emits lands here in real
 * time, with a connection-status badge that honestly reflects the
 * underlying transport (live / reconnecting / offline).
 *
 * Two reading modes share the same data:
 *
 *   - Activity (default, consumer view) — a curated, plain-English
 *     timeline. Each row is one user-meaningful action: "Plan
 *     drafted", "Started running", "Asked you for approval", "Sent
 *     0.5 PAX", "Signed receipt", final answer text inline. Protocol
 *     jargon (synth.*, walk.cortex.*, router.*, manifest.*) is
 *     filtered out — the user sees the *result*, not the machinery.
 *
 *   - Details (toggle, developer view) — the raw event stream as it
 *     was logged: phase pill + raw event.type + JSON detail one-liner.
 *     Useful for debugging a stuck run; never the default.
 *
 * The Plan → Gather → Execute → Verify → Sign stepper at the top is
 * driven LIVE off the SSE feed via deriveLiveStages(), so the user
 * sees stages light up the instant the daemon transitions, instead of
 * waiting for the next dashboard refetch (the seed stages mapped from
 * the polled IntentSummary are used only as the baseline and never
 * regress the stepper).
 */
import { useEffect, useMemo, useRef, useState } from 'react'
import { Radio, Loader2, CircleAlert, Sparkles, Wrench } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Button } from '@/components/ui/button'
import { StatusPill } from '@/components/matrix/status-pill'
import { RunPipeline } from '@/components/matrix/run-pipeline'
import { useRunEvents } from '@/hooks/api/useRunEvents'
import { cn } from '@/lib/utils'
import type { SSEEvent } from '@/lib/api/types'
import type { Run } from '@/lib/matrix-data'
import { deriveLiveStages } from '@/lib/api/mappers'

/** Maps a daemon event phase to a friendly label key + tone. The phase
 *  vocabulary mirrors executor/cmd/mcl-execute/daemon_events.go. Used
 *  only by the Details (raw) view; the consumer Activity view ignores
 *  phase and works off curated event-type translations instead. */
const PHASE_META: Record<string, { key: string; tone: string }> = {
  boot: { key: 'phaseBoot', tone: 'text-muted-foreground' },
  compile: { key: 'phasePlan', tone: 'text-chart-2' },
  synth: { key: 'phaseThink', tone: 'text-chart-2' },
  walk: { key: 'phaseExecute', tone: 'text-primary' },
  attest: { key: 'phaseSign', tone: 'text-primary' },
  envelope: { key: 'phaseRecord', tone: 'text-muted-foreground' },
  lifecycle: { key: 'phaseStatus', tone: 'text-muted-foreground' },
  infra: { key: 'phaseInfra', tone: 'text-muted-foreground' },
  http: { key: 'phaseNetwork', tone: 'text-muted-foreground' },
  snapshot: { key: 'phaseSnapshot', tone: 'text-muted-foreground' },
  sse: { key: 'phaseStream', tone: 'text-chart-3' },
  summary: { key: 'phaseStatus', tone: 'text-primary' },
}

/** Keys worth surfacing as the one-line detail in the raw view, in
 *  priority order. The list is broadened from the original snapshot so
 *  step.text (text), lifecycle.transition (to), envelope.signed (kind)
 *  and message.start (prose) all render their natural human payload. */
const DETAIL_KEYS = [
  'summary',
  'message',
  'note',
  'text',
  'prose',
  'reason',
  'error',
  'tool',
  'tool_name',
  'name',
  'kind',
  'to',
  'server',
  'model',
  'status',
  'verb',
  'skill',
  'count',
  'node_count',
]

function eventDetail(fields: Record<string, unknown> | undefined): string {
  if (!fields) return ''
  for (const k of DETAIL_KEYS) {
    const v = fields[k]
    if (v !== undefined && v !== null && v !== '') {
      const s = typeof v === 'string' ? v : String(v)
      // Truncate giant strings (LLM step.text can be long); the full
      // text is preserved on the Activity view's final-answer card.
      return s.length > 240 ? s.slice(0, 240) + '…' : s
    }
  }
  return ''
}

function formatTime(ts: string): string {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

/* ------------------------------------------------------------------ */
/* Activity (consumer) view — curated translation of daemon events.   */
/* ------------------------------------------------------------------ */

type ActivityKind = 'plan' | 'gather' | 'execute' | 'verify' | 'sign' | 'wallet' | 'gate' | 'note'

interface ActivityRow {
  /** Stable id used as React key. */
  id: string
  /** Original event timestamp, RFC3339Nano. */
  ts: string
  /** Bucket — drives icon + tone. */
  kind: ActivityKind
  /** Plain-English action label, eg "Plan drafted". */
  label: string
  /** Optional one-line context (truncated). */
  detail?: string
  /** Optional long body to render as a quoted card (final answer, etc). */
  body?: string
}

/**
 * Translate a single SSEEvent into an ActivityRow, or null to drop it
 * from the consumer view (jargon, telemetry, internal cortex/synth/
 * router noise). The whitelist below intentionally maps only events
 * that carry a user-meaningful semantic.
 */
function toActivityRow(ev: SSEEvent): ActivityRow | null {
  const f = ev.fields ?? {}
  const id = `${ev.seq}-${ev.type}`
  const ts = ev.ts

  switch (ev.type) {
    case 'message.start': {
      const prose = typeof f.prose === 'string' ? (f.prose as string) : ''
      return {
        id,
        ts,
        kind: 'plan',
        label: 'Got your request',
        detail: prose ? `“${prose}”` : undefined,
      }
    }
    case 'lifecycle.transition': {
      const to = typeof f.to === 'string' ? (f.to as string) : ''
      switch (to) {
        case 'proposed':
          return { id, ts, kind: 'plan', label: 'Plan drafted' }
        case 'clarifying':
          return { id, ts, kind: 'gate', label: 'Asked for clarification' }
        case 'accepted':
          return { id, ts, kind: 'gather', label: 'Plan accepted' }
        case 'executing':
          return { id, ts, kind: 'execute', label: 'Started running' }
        case 'completed':
          return { id, ts, kind: 'sign', label: 'Done' }
        case 'failed': {
          const reason = typeof f.error === 'string' ? (f.error as string) : ''
          return { id, ts, kind: 'note', label: 'Run failed', detail: reason || undefined }
        }
        case 'cancelled':
          return { id, ts, kind: 'note', label: 'Run cancelled' }
        default:
          return null
      }
    }
    case 'gate.invoked': {
      const tool = typeof f.tool === 'string' ? (f.tool as string) : ''
      return {
        id,
        ts,
        kind: 'gate',
        label: 'Waiting for your approval',
        detail: tool ? `for ${tool}` : undefined,
      }
    }
    case 'gate.decided': {
      const decision = typeof f.decision === 'string' ? (f.decision as string) : ''
      return {
        id,
        ts,
        kind: 'gate',
        label: decision === 'approve' ? 'Approval granted' : 'Approval declined',
      }
    }
    case 'paxeer.spend.evaluated': {
      const ok = f.allowed === true
      const amt = typeof f.amount === 'string' ? (f.amount as string) : ''
      return {
        id,
        ts,
        kind: 'wallet',
        label: ok ? 'Spend within your limit' : 'Spend exceeds your limit',
        detail: amt || undefined,
      }
    }
    case 'paxeer.spend.executed': {
      const amt = typeof f.amount === 'string' ? (f.amount as string) : ''
      const sym = typeof f.symbol === 'string' ? (f.symbol as string) : 'PAX'
      return {
        id,
        ts,
        kind: 'wallet',
        label: `Sent ${amt || ''} ${sym}`.trim(),
      }
    }
    case 'step.text': {
      // Final or intermediate model output. We surface it as a quoted
      // body card so the user actually sees the answer inline rather
      // than having to chase down the receipt.
      const text = typeof f.text === 'string' ? (f.text as string) : ''
      const node = typeof f.node_id === 'string' ? (f.node_id as string) : ''
      if (!text) return null
      return {
        id,
        ts,
        kind: 'execute',
        label: 'Agent answered',
        body: text,
        detail: node ? `step ${node}` : undefined,
      }
    }
    case 'walk.cortex.post': {
      // Verify boundary — pre/post cortex roots are compared. The
      // user-facing semantic is "we're verifying the run".
      return { id, ts, kind: 'verify', label: 'Verifying result' }
    }
    case 'envelope.signed': {
      const kind = typeof f.kind === 'string' ? (f.kind as string) : ''
      switch (kind) {
        case 'intent.attest':
          return { id, ts, kind: 'sign', label: 'Signed receipt' }
        case 'intent.fail':
          return { id, ts, kind: 'note', label: 'Recorded failure' }
        case 'intent.cancel':
          return { id, ts, kind: 'note', label: 'Recorded cancellation' }
        default:
          return null
      }
    }
    case 'intent.cost.summary': {
      const total = typeof f.total_pax === 'string' ? (f.total_pax as string) : ''
      if (!total) return null
      return {
        id,
        ts,
        kind: 'wallet',
        label: 'Total run cost',
        detail: `${total} PAX`,
      }
    }
    case 'message.complete': {
      const status = typeof f.status === 'string' ? (f.status as string) : ''
      const dur = typeof f.duration_ms === 'number' ? (f.duration_ms as number) : 0
      const durStr = dur > 0 ? formatDurationMs(dur) : ''
      if (status === 'completed') {
        return {
          id,
          ts,
          kind: 'sign',
          label: 'Finished',
          detail: durStr ? `in ${durStr}` : undefined,
        }
      }
      if (status === 'failed') {
        return { id, ts, kind: 'note', label: 'Finished with errors' }
      }
      return null
    }
    default:
      return null
  }
}

function formatDurationMs(ms: number): string {
  if (ms < 1000) return `${ms} ms`
  const s = Math.round(ms / 100) / 10
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  const rs = Math.round(s - m * 60)
  return `${m}m ${rs}s`
}

const ACTIVITY_TONE: Record<ActivityKind, { dot: string; text: string }> = {
  plan: { dot: 'bg-chart-2', text: 'text-chart-2' },
  gather: { dot: 'bg-chart-2', text: 'text-chart-2' },
  execute: { dot: 'bg-primary', text: 'text-primary' },
  verify: { dot: 'bg-primary', text: 'text-primary' },
  sign: { dot: 'bg-primary', text: 'text-primary' },
  wallet: { dot: 'bg-chart-3', text: 'text-chart-3' },
  gate: { dot: 'bg-chart-3', text: 'text-chart-3' },
  note: { dot: 'bg-muted-foreground', text: 'text-muted-foreground' },
}

/* ------------------------------------------------------------------ */
/* Component                                                          */
/* ------------------------------------------------------------------ */

export function RunTranscriptSheet({
  run,
  open,
  onOpenChange,
}: {
  run: Run | null
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const t = useTranslations('runTranscript')
  // Only subscribe while the sheet is open and a run is selected — the
  // SSE connection is torn down the moment it closes.
  const intentId = open && run ? run.id : null
  const stream = useRunEvents(intentId)

  const scrollRef = useRef<HTMLDivElement>(null)
  const eventCount = stream.events.length

  // Reading mode toggle. Defaults to the consumer Activity view; the
  // Details (raw) view is one click away for debugging.
  const [showRaw, setShowRaw] = useState(false)

  // Live-derived stage array. Falls back to the polled run.stages when
  // no events have arrived yet, then refines as events stream in. This
  // is what makes the Plan → Gather → Execute → Verify → Sign stepper
  // light up in real time instead of snapping at refetch.
  const liveStages = useMemo(() => {
    if (!run) return []
    return deriveLiveStages(run.stages, stream.events).stages
  }, [run, stream.events])

  // Curated activity rows for the consumer view.
  const activity = useMemo<ActivityRow[]>(() => {
    const out: ActivityRow[] = []
    for (const ev of stream.events) {
      const row = toActivityRow(ev)
      if (row) out.push(row)
    }
    return out
  }, [stream.events])

  // Keep the newest row in view as the stream advances.
  useEffect(() => {
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [activity.length, eventCount, showRaw])

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="flex w-full flex-col gap-0 overflow-hidden p-0 sm:max-w-lg">
        <SheetHeader className="border-border shrink-0 border-b">
          <div className="flex items-center justify-between gap-3">
            <div className="flex items-center gap-2">
              <Radio className="text-primary size-5" />
              <SheetTitle className="text-sm">{t('title')}</SheetTitle>
            </div>
            <ConnectionBadge state={stream.connection} attempt={stream.reconnectAttempt} />
          </div>
          <SheetDescription className="text-pretty">{run?.title ?? t('noRun')}</SheetDescription>
          {run && (
            <div className="flex items-center gap-2 pt-1">
              <StatusPill status={run.status} />
              <span className="text-muted-foreground font-mono text-xs">{run.id}</span>
            </div>
          )}
        </SheetHeader>

        {run && (
          <div className="border-border max-h-[38vh] shrink-0 overflow-y-auto border-b px-4 py-3">
            <RunPipeline stages={liveStages} showLabels />
          </div>
        )}

        <div className="border-border flex shrink-0 items-center justify-between gap-2 border-b px-4 py-2">
          <span className="text-muted-foreground text-[11px] font-medium tracking-wide uppercase">
            {showRaw ? t('detailsHeader') : t('activityHeader')}
          </span>
          <Button
            variant="ghost"
            size="sm"
            className="h-7 gap-1.5 px-2 text-xs"
            onClick={() => setShowRaw((v) => !v)}
            aria-pressed={showRaw}
          >
            {showRaw ? (
              <>
                <Sparkles className="size-3.5" />
                {t('showActivity')}
              </>
            ) : (
              <>
                <Wrench className="size-3.5" />
                {t('showDetails')}
              </>
            )}
          </Button>
        </div>

        <div ref={scrollRef} className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
          {stream.connection === 'error' ? (
            <EmptyState
              icon={CircleAlert}
              tone="text-destructive"
              title={t('errorTitle')}
              desc={stream.lastError ?? t('errorDesc')}
            />
          ) : showRaw ? (
            eventCount === 0 ? (
              <EmptyState
                icon={stream.connection === 'open' ? Radio : Loader2}
                tone="text-muted-foreground"
                spin={stream.connection !== 'open'}
                title={stream.connection === 'open' ? t('waitingTitle') : t('connectingTitle')}
                desc={stream.connection === 'open' ? t('waitingDesc') : t('connectingDesc')}
              />
            ) : (
              <ol className="flex flex-col gap-2">
                {stream.events.map((ev, i) => (
                  <RawEventRow key={`${ev.seq}-${i}`} event={ev} />
                ))}
              </ol>
            )
          ) : activity.length === 0 ? (
            <EmptyState
              icon={stream.connection === 'open' ? Radio : Loader2}
              tone="text-muted-foreground"
              spin={stream.connection !== 'open'}
              title={stream.connection === 'open' ? t('waitingTitle') : t('connectingTitle')}
              desc={stream.connection === 'open' ? t('waitingDesc') : t('connectingDesc')}
            />
          ) : (
            <ol className="flex flex-col gap-2">
              {activity.map((row) => (
                <ActivityRowView key={row.id} row={row} />
              ))}
            </ol>
          )}
        </div>
      </SheetContent>
    </Sheet>
  )
}

function ConnectionBadge({
  state,
  attempt,
}: {
  state: ReturnType<typeof useRunEvents>['connection']
  attempt: number
}) {
  const t = useTranslations('runTranscript')
  const meta: Record<typeof state, { label: string; tone: string; dot: string; pulse?: boolean }> =
    {
      connecting: {
        label: t('connecting'),
        tone: 'bg-chart-3/12 text-chart-3',
        dot: 'bg-chart-3',
        pulse: true,
      },
      open: {
        label: t('live'),
        tone: 'bg-primary/12 text-primary',
        dot: 'bg-primary',
        pulse: true,
      },
      reconnecting: {
        label: t('reconnecting', { attempt }),
        tone: 'bg-chart-3/12 text-chart-3',
        dot: 'bg-chart-3',
        pulse: true,
      },
      error: {
        label: t('offline'),
        tone: 'bg-destructive/12 text-destructive',
        dot: 'bg-destructive',
      },
      closed: {
        label: t('closed'),
        tone: 'bg-muted text-muted-foreground',
        dot: 'bg-muted-foreground',
      },
    }
  const m = meta[state]
  return (
    <span
      className={cn(
        'inline-flex shrink-0 items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium',
        m.tone,
      )}
    >
      <span className={cn('size-1.5 rounded-full', m.dot, m.pulse && 'animate-pulse')} />
      {m.label}
    </span>
  )
}

function ActivityRowView({ row }: { row: ActivityRow }) {
  const tone = ACTIVITY_TONE[row.kind]
  return (
    <li className="border-border bg-card flex items-start gap-3 rounded-lg border p-3">
      <span className={cn('mt-1 size-1.5 shrink-0 rounded-full', tone.dot)} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <span className={cn('text-sm font-medium', tone.text)}>{row.label}</span>
          <span className="text-muted-foreground/60 ml-auto font-mono text-[11px]">
            {formatTime(row.ts)}
          </span>
        </div>
        {row.detail && (
          <p className="text-muted-foreground mt-0.5 text-xs text-pretty">{row.detail}</p>
        )}
        {row.body && (
          <pre className="border-border bg-muted/40 text-foreground/90 mt-2 max-h-64 overflow-y-auto rounded-md border p-2 font-sans text-xs whitespace-pre-wrap">
            {row.body}
          </pre>
        )}
      </div>
    </li>
  )
}

function RawEventRow({ event }: { event: SSEEvent }) {
  const t = useTranslations('runTranscript')
  const meta = PHASE_META[event.phase] ?? { key: 'phaseUnknown', tone: 'text-muted-foreground' }
  const detail = useMemo(() => eventDetail(event.fields), [event.fields])
  const isGap = event.type === 'sse.gap'

  if (isGap) {
    return (
      <li className="flex items-center gap-2 py-1">
        <span className="bg-border h-px flex-1" />
        <span className="text-muted-foreground/70 text-[11px]">{t('reconciled')}</span>
        <span className="bg-border h-px flex-1" />
      </li>
    )
  }

  return (
    <li className="border-border bg-card flex items-start gap-3 rounded-lg border p-3">
      <span className={cn('mt-1 size-1.5 shrink-0 rounded-full bg-current', meta.tone)} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <span className={cn('text-sm font-medium', meta.tone)}>{t(meta.key)}</span>
          <span className="text-muted-foreground font-mono text-[11px]">{event.type}</span>
          <span className="text-muted-foreground/60 ml-auto font-mono text-[11px]">
            {formatTime(event.ts)}
          </span>
        </div>
        {detail && <p className="text-foreground/90 mt-0.5 text-xs text-pretty">{detail}</p>}
      </div>
    </li>
  )
}

function EmptyState({
  icon: Icon,
  tone,
  title,
  desc,
  spin,
}: {
  icon: React.ComponentType<{ className?: string }>
  tone: string
  title: string
  desc: string
  spin?: boolean
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-16 text-center">
      <Icon className={cn('size-6', tone, spin && 'animate-spin')} />
      <p className="text-foreground text-sm font-medium">{title}</p>
      <p className="text-muted-foreground max-w-xs text-xs text-pretty">{desc}</p>
    </div>
  )
}
