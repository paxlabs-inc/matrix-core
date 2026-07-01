'use client'

/**
 * ActivityRegion — the `activity` zone of the OS shell (R17.2, R17.3 host).
 *
 * This is where the live `Timeline` of Neo's activity lives: glanceable
 * awareness of what Neo is doing right now (design "Surface → OS concept
 * mapping": Timeline → activity / task center, "Glanceable awareness of what
 * Neo is doing"). The placement policy routes every `timeline` surface here
 * (`REGION_BY_KIND` in `lib/construct/workspace.ts`), so this region simply
 * lays out whatever Timeline surface(s) the workspace has placed in it.
 *
 * It REUSES the frozen per-primitive renderers untouched: each placed surface
 * is rendered through `SurfaceRenderer`, which dispatches a `timeline` surface
 * to the existing `TimelineView` (R6.2/R6.4 — the shell arranges, it never
 * synthesizes UI). This component contributes activity-region CHROME only (a
 * glanceable, non-jargon label and background-tone separation); it never
 * reaches into the renderer.
 *
 * In-place patching (R16.4): every surface is keyed by its STABLE surface id,
 * and the region wrapper itself is a stable element. When a
 * `construct.surface.patch` folds into a Timeline, `applySurfaceEvent` replaces
 * only that one `PlacedSurface` and carries the rest of the workspace by
 * reference; with stable keys React updates the targeted Timeline's subtree in
 * place and never remounts the region or re-lays-out the rest of the shell.
 *
 * House rules (R13): separation is by background tone only — no border strokes,
 * no shadow, no glow; no emojis, no purple gradients; copy is consumer-readable
 * with no protocol jargon (R12). The shell's SINGLE accent is Paxeer Blue
 * (`text-pax` / `#004CED`): the per-step "View detail" descent affordance lifts
 * to it on hover/focus, and no competing accent is introduced.
 */
import { cn } from '@/lib/utils'
import { SurfaceRenderer, type SurfaceHandlers } from '@/components/matrix/construct'
import { resolveLink, type Linkable } from '@/lib/construct/focus'
import type { PlacedSurface } from '@/lib/construct/workspace'
import type { TimelineStep } from '@/lib/construct/types.gen'

/** Consumer-readable label for the activity region (no protocol jargon — R12). */
const ACTIVITY_LABEL = "Neo's activity"

/**
 * StepDescentControls — the shell-level tap affordances for descending from a
 * Timeline step into its linked `Stream` (R4.3, R17.3/R17.4).
 *
 * The frozen `TimelineView` renderer draws the steps but exposes no per-step tap
 * (it is reused untouched — R6.2/R6.4). Depth navigation is a SHELL concern, so
 * the shell contributes its own ARRANGE-only chrome here: for each step that
 * carries a `ref`/`parent` link, a plain, consumer-readable control that calls
 * `onDescendStep` with that step. Steps with no link carry no control (tapping
 * them would be a no-op — R4.6), so the affordance only appears where there is
 * detail to open. This synthesizes no surface content; it only wires the tap.
 */
function StepDescentControls({
  steps,
  onDescendStep,
}: {
  steps: TimelineStep[]
  onDescendStep: (step: Linkable) => void
}) {
  const linkable = steps.filter((s) => resolveLink(s) !== undefined)
  if (linkable.length === 0) return null
  return (
    <div className="flex flex-col gap-1">
      {linkable.map((step) => (
        <button
          key={step.id}
          type="button"
          onClick={() => onDescendStep(step)}
          className="text-muted-foreground hover:text-pax focus-visible:text-pax self-start text-xs font-medium transition-colors"
        >
          {`View detail: ${step.label}`}
        </button>
      ))}
    </div>
  )
}

export function ActivityRegion({
  surfaces,
  handlers,
  onDescendStep,
  className,
}: {
  /** The surfaces the workspace has placed in the `activity` region (Timelines). */
  surfaces: PlacedSurface[]
  /** Surface interaction handlers threaded through to the reused renderers. */
  handlers?: SurfaceHandlers
  /**
   * Descend from a tapped Timeline step into its linked `Stream` at raw (R17.4).
   * When omitted, the activity region shows the Timeline without descent chrome.
   */
  onDescendStep?: (step: Linkable) => void
  className?: string
}) {
  // Nothing placed here yet: render nothing so the stage stays clean (the region
  // appears the moment Neo's activity Timeline is placed).
  if (surfaces.length === 0) return null

  return (
    <section
      data-region="activity"
      aria-label={ACTIVITY_LABEL}
      className={cn('flex flex-col gap-2', className)}
    >
      <p className="text-muted-foreground/70 font-mono text-[0.6rem] tracking-[0.14em] uppercase">
        {ACTIVITY_LABEL}
      </p>
      <div className="flex flex-col gap-3">
        {surfaces.map((p) => (
          // Stable key by surface id: a patch updates THIS Timeline's subtree in
          // place (no remount, no full re-layout — R16.4). The Timeline is drawn
          // by the existing frozen renderer (TimelineView via SurfaceRenderer),
          // untouched.
          <div key={p.surface.id} className="flex flex-col gap-1.5">
            <SurfaceRenderer surface={p.surface} handlers={handlers} />
            {onDescendStep && p.surface.timeline?.steps && (
              <StepDescentControls steps={p.surface.timeline.steps} onDescendStep={onDescendStep} />
            )}
          </div>
        ))}
      </div>
    </section>
  )
}
