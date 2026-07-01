'use client'

/**
 * Timeline — structured, STATEFUL steps over time (plan execution, async jobs,
 * lifecycle, a swarm). Distinct from Stream: each step carries state
 * (status/result), not just append-only bytes. Rendered as a connected rail
 * where each node's glyph reflects pending/running/done/failed.
 */
import { Check, CircleX, Loader2 } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type {
  Timeline as TimelinePayload,
  TimelineStep,
  StepStatus,
} from '@/lib/construct/types.gen'

function StatusNode({ status }: { status: StepStatus }) {
  if (status === 'running') {
    return (
      <span className="bg-background grid size-5 place-items-center rounded-full">
        <Loader2 className="text-primary size-3.5 animate-spin" />
      </span>
    )
  }
  if (status === 'failed') {
    return (
      <span className="bg-background grid size-5 place-items-center rounded-full">
        <CircleX className="text-destructive size-3.5" />
      </span>
    )
  }
  if (status === 'done') {
    return (
      <span className="bg-background grid size-5 place-items-center rounded-full">
        <Check className="text-primary size-3.5" />
      </span>
    )
  }
  return (
    <span className="bg-background grid size-5 place-items-center rounded-full">
      <span className="bg-foreground/25 size-2 rounded-full" />
    </span>
  )
}

function StepRow({ step, last }: { step: TimelineStep; last: boolean }) {
  return (
    <div className="flex gap-3">
      <div className="flex flex-col items-center">
        <StatusNode status={step.status} />
        {!last && <span className="bg-foreground/10 w-px flex-1" />}
      </div>
      <div className={cn('min-w-0 flex-1', last ? 'pb-0' : 'pb-3')}>
        <p
          className={cn(
            'text-sm leading-tight font-medium',
            step.status === 'pending' ? 'text-muted-foreground' : 'text-foreground',
          )}
        >
          {step.label}
        </p>
        {step.detail && (
          <p className="text-muted-foreground/80 mt-0.5 text-xs leading-snug">{step.detail}</p>
        )}
      </div>
    </div>
  )
}

export function TimelineView({ timeline }: { timeline: TimelinePayload }) {
  if (timeline.steps.length === 0) return null
  return (
    <div className="bg-foreground/[0.03] rounded-2xl px-4 py-3.5">
      {timeline.title && (
        <p className="text-muted-foreground/70 mb-3 font-mono text-[0.6rem] tracking-[0.14em] uppercase">
          {timeline.title}
        </p>
      )}
      <div className="flex flex-col">
        {timeline.steps.map((s, i) => (
          <StepRow key={s.id} step={s} last={i === timeline.steps.length - 1} />
        ))}
      </div>
    </div>
  )
}
