import { cn } from '@/lib/utils'
import { STATUS_LABEL, type RunStatus } from '@/lib/matrix-data'

/**
 * StatusPill — semantic, color + icon (never color alone) status indicator.
 * Backend-agnostic: takes a RunStatus literal only.
 */
const TONES: Record<RunStatus, string> = {
  running: 'border-chart-2/30 bg-chart-2/10 text-chart-2',
  needs_input: 'border-chart-3/30 bg-chart-3/10 text-chart-3',
  completed: 'border-primary/30 bg-primary/10 text-primary',
  failed: 'border-destructive/30 bg-destructive/10 text-destructive',
  queued: 'border-border bg-muted text-muted-foreground',
  paused: 'border-border bg-muted text-muted-foreground',
}

export function StatusPill({ status, className }: { status: RunStatus; className?: string }) {
  const pulsing = status === 'running'
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 font-mono text-xs font-medium',
        TONES[status],
        className,
      )}
    >
      <span
        className={cn('size-1.5 rounded-full bg-current', pulsing && 'animate-pulse')}
        aria-hidden="true"
      />
      {STATUS_LABEL[status]}
    </span>
  )
}
