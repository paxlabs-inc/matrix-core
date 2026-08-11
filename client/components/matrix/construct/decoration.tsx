'use client'

/**
 * Attribute decoration — the axes that DECORATE a primitive rather than
 * becoming one (frozen spec §attributes, invariant i4): stakes, confidence,
 * cost, temporality. Rendered as quiet, tone-only chips above a surface; the
 * single accent is Centra Sage (#99bd9c via --primary), irreversible borrows
 * the destructive tone, nothing uses borders/glow/purple.
 */
import type { Attributes, Stakes, Cost } from '@/lib/construct/types.gen'

const STAKES_TONE: Record<Stakes, string> = {
  fact: 'bg-foreground/[0.06] text-muted-foreground',
  hypothesis: 'bg-foreground/[0.05] text-muted-foreground italic',
  decision: 'bg-primary/12 text-primary',
  irreversible: 'bg-destructive/12 text-destructive',
}

const STAKES_LABEL: Record<Stakes, string> = {
  fact: 'fact',
  hypothesis: 'hypothesis',
  decision: 'decision',
  irreversible: 'irreversible',
}

function Chip({ tone, children }: { tone: string; children: React.ReactNode }) {
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 font-mono text-[0.6rem] tracking-[0.08em] uppercase ${tone}`}
    >
      {children}
    </span>
  )
}

export function StakesBadge({ stakes }: { stakes: Stakes }) {
  return <Chip tone={STAKES_TONE[stakes]}>{STAKES_LABEL[stakes]}</Chip>
}

export function CostChip({ cost }: { cost: Cost }) {
  const unit = cost.unit || 'PAX'
  const capped = typeof cost.cap === 'number' && cost.cap > 0
  return (
    <Chip tone="bg-foreground/[0.06] text-foreground/80">
      {cost.amount} {unit}
      {capped ? ` / ${cost.cap}` : ''}
    </Chip>
  )
}

export function ConfidenceMeter({ value }: { value: number }) {
  const pct = Math.max(0, Math.min(1, value))
  return (
    <span
      className="inline-flex items-center gap-1.5"
      title={`confidence ${Math.round(pct * 100)}%`}
    >
      <span className="bg-foreground/[0.08] relative block h-1 w-10 overflow-hidden rounded-full">
        <span
          className="bg-primary absolute inset-y-0 left-0 rounded-full"
          style={{ width: `${pct * 100}%` }}
        />
      </span>
      <span className="text-muted-foreground/80 font-mono text-[0.6rem]">
        {Math.round(pct * 100)}%
      </span>
    </span>
  )
}

/** True when an attribute set carries any decoration worth a header row. */
export function hasDecoration(attrs?: Attributes): boolean {
  if (!attrs) return false
  return (
    !!attrs.stakes ||
    !!attrs.cost ||
    typeof attrs.confidence === 'number' ||
    (!!attrs.temporality && attrs.temporality !== 'point')
  )
}

/**
 * DecorationRow — the optional chip strip a SurfaceRenderer paints above a
 * primitive when its attributes carry meaning. Renders nothing for a bare
 * surface, so the common case stays clean.
 */
export function DecorationRow({ attributes }: { attributes?: Attributes }) {
  if (!hasDecoration(attributes) || !attributes) return null
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {attributes.stakes && <StakesBadge stakes={attributes.stakes} />}
      {attributes.cost && <CostChip cost={attributes.cost} />}
      {typeof attributes.confidence === 'number' && (
        <ConfidenceMeter value={attributes.confidence} />
      )}
      {attributes.temporality === 'persistent' && (
        <Chip tone="bg-foreground/[0.05] text-muted-foreground">persistent</Chip>
      )}
    </div>
  )
}
