'use client'

/**
 * RawFocusOverlay — renders the surface the depth stack is FOCUSED on, at its
 * raw level, as a region layered over the environment stage (design "Depth
 * navigation", R4.9, R6.2/R6.4, R17.3).
 *
 * Depth is a SHELL concern: when a Timeline step is tapped, the shell pushes a
 * raw `FocusFrame` targeting that step's linked `Stream` (see
 * `lib/construct/focus.ts`). This overlay reads the top frame from the shared
 * `SurfaceWorkspace`, finds the targeted `PlacedSurface`, and draws it through
 * the EXISTING frozen per-primitive renderer untouched (`SurfaceRenderer` →
 * `StreamView` for a Stream). The shell arranges and provides the ascend chrome;
 * it never reaches into the renderer and never synthesizes the surface content
 * (R6.4).
 *
 * When the depth stack is at the base (no focus frame), or the targeted surface
 * is absent, this renders nothing — the environment stage shows through.
 *
 * House rules (R13) + non-jargon copy (R12): separation from the stage is by
 * background tone only (no border strokes, no shadow, no glow); the ascend
 * control is a plain, consumer-readable "Back" affordance with no protocol
 * jargon; no emojis, no purple gradients. Paxeer Blue (`text-pax` / `#004CED`)
 * is the shell's SINGLE accent and is the one color the ascend control lifts to
 * on hover/focus; no other accent is introduced.
 */
import { ArrowLeft } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import { SurfaceRenderer, type SurfaceHandlers } from '@/components/matrix/construct'
import { topFocus } from '@/lib/construct/focus'
import type { SurfaceWorkspace } from '@/lib/construct/workspace'

/** Consumer-readable label for the ascend (back) control (no jargon — R12). */
const BACK_LABEL = 'Back'

export function RawFocusOverlay({
  workspace,
  onAscend,
  handlers,
  className,
}: {
  /** The shared surface-state model; its focus stack drives what is shown. */
  workspace: SurfaceWorkspace
  /** Pop the top focus frame (ascend). Wired by the shell owner. */
  onAscend?: () => void
  /** Surface interaction handlers threaded through to the reused renderer. */
  handlers?: SurfaceHandlers
  className?: string
}) {
  const frame = topFocus(workspace)
  // Base of the stack: nothing focused → render nothing (the stage shows).
  if (!frame) return null

  const focused = workspace.surfaces.get(frame.surfaceId)
  // Targeted surface absent (e.g. aged out): render nothing rather than an empty
  // shell — the focus simply has nothing to show.
  if (!focused) return null

  return (
    <section
      data-shell-overlay
      data-focus-level={frame.level}
      aria-label="Detail view"
      // bg-background layers this region OVER the stage; the tone difference is
      // the only separation (no border strokes — R13.1/R13.2).
      className={cn('bg-background absolute inset-0 z-20 flex flex-col overflow-hidden', className)}
    >
      <div className="flex items-center gap-2 px-3 py-2.5">
        <button
          type="button"
          onClick={onAscend}
          aria-label={BACK_LABEL}
          className="text-muted-foreground hover:text-pax focus-visible:text-pax inline-flex items-center gap-1.5 text-sm font-medium transition-colors"
        >
          <ArrowLeft className="size-4" />
          {BACK_LABEL}
        </button>
      </div>
      {/* The focused surface, drawn by the reused frozen renderer untouched. */}
      <div className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <SurfaceRenderer surface={focused.surface} handlers={handlers} />
      </div>
    </section>
  )
}
