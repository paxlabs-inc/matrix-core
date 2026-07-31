'use client'

/**
 * Metric — a named value with optional unit / trend / threshold.
 *
 * The value is the hero (large, tabular mono); the label sits above it small.
 * A trend tints the delta (up/down/flat) and a threshold paints a quiet
 * fill bar with warn/limit ticks. Covers datum, cost, budget, progress.
 */
import { ArrowUp, ArrowRight, ChevronDown } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import { GaugeChart } from '@/components/ui/gauge-chart'
import type { Metric as MetricPayload, Trend } from '@/lib/construct/types.gen'

const UP = 'oklch(0.72 0.14 155)'
const DOWN = 'oklch(0.62 0.2 25)'

function TrendGlyph({ trend }: { trend: Trend }) {
  if (trend === 'up') return <ArrowUp className="size-3.5" style={{ color: UP }} />
  if (trend === 'down') return <ChevronDown className="size-3.5" style={{ color: DOWN }} />
  return <ArrowRight className="text-muted-foreground size-3.5" />
}

function ThresholdBar({ value, warn, limit }: { value: number; warn?: number; limit?: number }) {
  const max = Math.max(value, limit ?? 0, warn ?? 0) || 1
  const pct = Math.max(0, Math.min(1, value / max))
  const over = limit !== undefined && value >= limit
  const near = warn !== undefined && value >= warn
  const fill = over ? DOWN : near ? 'oklch(0.78 0.15 85)' : 'var(--primary)'
  return (
    <span className="bg-foreground/[0.08] relative mt-2 block h-1.5 w-full overflow-hidden rounded-full">
      <span
        className="absolute inset-y-0 left-0 rounded-full transition-[width]"
        style={{ width: `${pct * 100}%`, background: fill }}
      />
      {warn !== undefined && (
        <span
          className="bg-background/70 absolute inset-y-0 w-px"
          style={{ left: `${(warn / max) * 100}%` }}
        />
      )}
      {limit !== undefined && (
        <span
          className="bg-background absolute inset-y-0 w-px"
          style={{ left: `${(limit / max) * 100}%` }}
        />
      )}
    </span>
  )
}

/**
 * GaugeView — the radial-dial display (display=gauge), delegating to the exact
 * GAIA GaugeChart fed by metric data. Absolute warn/limit thresholds are
 * converted to the 0..100 percentages the gauge expects.
 */
function GaugeView({ metric }: { metric: MetricPayload }) {
  const magnitude = metric.magnitude as number
  const limit = metric.threshold?.limit
  const max = limit && limit > 0 ? limit : Math.max(magnitude, 1)
  const toPct = (v?: number) => (v !== undefined && max ? Math.round((v / max) * 100) : undefined)
  const warning = toPct(metric.threshold?.warn)
  const danger = toPct(limit)
  return (
    <GaugeChart
      value={magnitude}
      title={metric.label}
      unit={metric.unit}
      max={max}
      thresholds={warning !== undefined && danger !== undefined ? { warning, danger } : undefined}
    />
  )
}

export function MetricView({ metric }: { metric: MetricPayload }) {
  if (metric.display === 'gauge' && typeof metric.magnitude === 'number') {
    return <GaugeView metric={metric} />
  }
  const hasThreshold =
    metric.display !== 'plain' &&
    metric.threshold &&
    typeof metric.magnitude === 'number' &&
    (metric.threshold.warn !== undefined || metric.threshold.limit !== undefined)
  return (
    <div className="bg-foreground/[0.03] rounded-2xl px-4 py-3.5">
      <div className="flex items-center justify-between gap-2">
        <p className="text-muted-foreground/80 truncate text-xs font-medium">{metric.label}</p>
        {metric.trend && <TrendGlyph trend={metric.trend} />}
      </div>
      <div className="mt-1 flex items-baseline gap-1.5">
        <span className="text-foreground font-mono text-2xl leading-none font-semibold tabular-nums">
          {metric.value}
        </span>
        {metric.unit && <span className="text-muted-foreground text-sm">{metric.unit}</span>}
        {metric.scale && (
          <span className="text-muted-foreground/70 text-xs font-medium">{metric.scale}</span>
        )}
      </div>
      {hasThreshold && (
        <ThresholdBar
          value={metric.magnitude as number}
          warn={metric.threshold?.warn}
          limit={metric.threshold?.limit}
        />
      )}
      {hasThreshold && metric.threshold?.limit !== undefined && (
        <p
          className={cn(
            'mt-1.5 text-[0.68rem]',
            (metric.magnitude as number) >= metric.threshold.limit
              ? 'text-destructive'
              : 'text-muted-foreground/70',
          )}
        >
          limit {metric.threshold.limit}
          {metric.unit ? ` ${metric.unit}` : ''}
        </p>
      )}
    </div>
  )
}
