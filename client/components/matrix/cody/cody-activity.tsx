'use client'

/**
 * Cody Activity Spine (cody-smoothness task 4.1) — the always-live progress
 * region. Rendered from the run's folded `activity` (milestone run.activity
 * events + the synthesized 'continuing' state), it shows the current phase in
 * register-appropriate plain language, a liveness indicator, elapsed time
 * (ticked locally from the activity's `since` timestamp), and Cody's latest
 * line. It is NEVER blank while the run is running or awaiting input — from
 * the first second of a dispatch (before any milestone) through the
 * understand/stack/plan/design/work/verify/preview phases and the
 * approve->continuing gap. Layers separate by background tone; no borders,
 * one accent.
 */
import { useEffect, useState } from 'react'

import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconClock } from '@/components/matrix/cody/icons'
import type { Register } from '@/components/matrix/cody/tiering'
import type { CodyRun } from '@/hooks/api/useCody'

/** Register-appropriate phase copy; falls back to the event's own label. */
const PHASE_COPY: Record<string, { outcome: string; technical: string }> = {
  understanding: { outcome: 'Getting to know your project', technical: 'Reading your workspace' },
  stack: { outcome: 'Choosing the best tools', technical: 'Weighing stack options' },
  planning: { outcome: 'Mapping out the work', technical: 'Planning the work' },
  design: { outcome: 'Designing the look', technical: 'Designing the interface' },
  working: { outcome: 'Building', technical: 'Building' },
  verifying: { outcome: 'Double-checking the work', technical: 'Verifying the work' },
  previewing: { outcome: 'Preparing your preview', technical: 'Preparing a preview' },
  continuing: { outcome: 'Continuing', technical: 'Continuing' },
}

export function activityLabel(run: CodyRun, register: Register): string {
  if (run.status === 'needs_input') {
    return register === 'outcome' ? 'Waiting on you' : 'Waiting for your input'
  }
  const phase = run.activity?.phase
  if (phase && PHASE_COPY[phase]) return PHASE_COPY[phase][register]
  if (run.activity?.label) return run.activity.label
  // Dispatch acknowledged but no milestone yet — never blank (req 1.1).
  return register === 'outcome' ? 'Cody is on it' : 'Starting the run'
}

function formatElapsed(ms: number): string {
  const totalSecs = Math.max(0, Math.floor(ms / 1000))
  const mins = Math.floor(totalSecs / 60)
  const secs = totalSecs % 60
  if (mins >= 60) {
    const hrs = Math.floor(mins / 60)
    return `${hrs}h ${mins % 60}m`
  }
  if (mins > 0) return `${mins}m ${secs}s`
  return `${secs}s`
}

/** Current elapsed = the event's elapsed_ms plus local time since it landed. */
function currentElapsedMs(run: CodyRun, now: number): number | null {
  const a = run.activity
  if (!a?.since) return null
  const since = Date.parse(a.since)
  if (Number.isNaN(since)) return typeof a.elapsedMs === 'number' ? a.elapsedMs : null
  return (a.elapsedMs ?? 0) + Math.max(0, now - since)
}

export function CodyActivitySpine({ run, register }: { run: CodyRun; register: Register }) {
  const live = run.status === 'running'
  const awaiting = run.status === 'needs_input'
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    if (!live) return
    const t = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(t)
  }, [live])

  if (!live && !awaiting) return null

  const label = activityLabel(run, register)
  const detail = run.activity?.detail ?? ''
  const elapsed = live ? currentElapsedMs(run, now) : null
  const lastLine = run.answer

  return (
    <section
      data-testid="cody-activity-spine"
      className="bg-surface-secondary flex shrink-0 items-center gap-3 rounded-lg px-4 py-3"
    >
      {live ? (
        <CodyLoader variant="ring" size={20} />
      ) : (
        <IconClock className="text-muted-foreground size-5 shrink-0" />
      )}
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <div className="flex items-baseline gap-2">
          <span className="truncate text-sm font-medium">{label}</span>
          {detail ? <span className="text-muted-foreground truncate text-sm">{detail}</span> : null}
          {elapsed != null ? (
            <span className="text-muted-foreground ml-auto shrink-0 font-mono text-[11px]">
              {formatElapsed(elapsed)}
            </span>
          ) : null}
        </div>
        {lastLine ? <p className="text-muted-foreground truncate text-xs">{lastLine}</p> : null}
      </div>
    </section>
  )
}
