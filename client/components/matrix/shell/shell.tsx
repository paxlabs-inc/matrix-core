'use client'

/**
 * Shell — the Construct OS shell (Desktop Web adapter), mounted at the client
 * route ROOT. This is the frame inversion (design "Frame inversion", R3.1/R3.2/
 * R3.4/R3.5, R17.4/R17.5): the ENVIRONMENT is the page, and the conversation
 * with Neo is exactly ONE panel occupying the `narration` region within it —
 * never the page that contains the environment.
 *
 * The shell reads the shared `SurfaceWorkspace` (the single client-side model
 * both adapters consume) and lays placed surfaces out BY REGION, rendering each
 * one through the existing frozen per-primitive renderers untouched
 * (`SurfaceRenderer`). It arranges; it never synthesizes UI (R6.4).
 *
 * Invariants this component holds:
 *   - The environment is the ROOT element of the route (R3.1). The narration
 *     region is always a DESCENDANT of it, never an ancestor (R3.4).
 *   - With zero `Narration` surfaces present, the shell STILL renders the
 *     environment as the root and keeps chat as a contained region — it never
 *     falls back to chat-as-page (R3.5).
 *   - Separation between the narration panel and the environment stage — and
 *     elevation of the descent notice above it — is by background TONE only
 *     (background → card → popover), never a border stroke, shadow, or glow
 *     (R13.1/R13.2/R13.5); the shell's single accent is Centra Sage `#99bd9c`
 *     (including the legacy `text-pax` alias), expressed only on interactive chrome, and copy carries no
 *     protocol jargon (R12, R13.6).
 *
 * Scope note (task 6.1 → 6.2): this lays the root frame + the narration panel,
 * and renders the live activity Timeline in the `activity` region through the
 * reused renderer (task 6.2). The remaining regions (descent to raw, the Metric
 * tray, the Ask inbox) are filled by later tasks; here they are real region containers that
 * render whatever the workspace has placed in them (empty today), so the
 * environment-as-root frame is genuine and ready for those surfaces. The live
 * feed → model wiring is task 8.1; until then the shell reads whatever
 * workspace it is handed.
 */
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Monitor, X } from 'lucide-react'
import { motion } from 'motion/react'
import { Button } from '@astryxdesign/core/Button'
import { cn } from '@/lib/utils'
import { motionTransition } from '@/lib/motion'
import { groupByRegion, type Region, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { SurfaceRenderer, type SurfaceHandlers } from '@/components/matrix/construct'
import { NarrationPanel } from '@/components/matrix/shell/narration-panel'
import { ActivityRegion } from '@/components/matrix/shell/activity-region'
import { RawFocusOverlay } from '@/components/matrix/shell/raw-focus-overlay'
import type { Linkable } from '@/lib/construct/focus'

/**
 * The environment-stage regions (every region except `narration`), in the order
 * they stack on the stage. `narration` is laid out separately as its own panel,
 * so it is intentionally absent here.
 */
const ENV_REGIONS: readonly Region[] = ['tray', 'activity', 'window', 'home', 'drawer', 'inbox']

export function Shell({
  workspace,
  narration,
  environment,
  handlers,
  environmentActive,
  onDescendStep,
  onAscend,
  descentNotice,
  onDismissNotice,
}: {
  /** The shared surface-state model the shell reads (fed live in task 8.1). */
  workspace: SurfaceWorkspace
  /** The chat thread, mounted as the `narration` region's content. */
  narration: ReactNode
  /** Neo's live work centerpiece (the relocated "Neo's Computer" — legacy
   *  NeoStep work not yet projected as typed Construct surfaces). Rendered on
   *  the environment stage alongside the typed surfaces, never inside the
   *  narration panel. When present it makes the stage appear even before any
   *  typed surface lands, so the user always sees Neo at work — never just a
   *  chat with nothing else. */
  environment?: ReactNode
  /**
   * Whether the environment node currently holds REAL work (a run's steps, a
   * booted desktop, a live surface) as opposed to merely being mounted.
   *
   * The Computer is always mounted so its power controls are one click away,
   * which used to mean the app opened onto an empty stage standing open with
   * nothing in it. The stage now starts CLOSED and opens itself the first time
   * there is something to show; the reopen control stays available throughout,
   * and an explicit close by the user sticks.
   */
  environmentActive?: boolean
  /** Surface interaction handlers (e.g. answering an Ask) threaded to renderers. */
  handlers?: SurfaceHandlers
  /**
   * Descend from a tapped Timeline step into its linked `Stream` at raw (R17.4).
   * Wired by the shell owner to the focus orchestration (`useDescent`).
   */
  onDescendStep?: (step: Linkable) => void
  /** Pop the top focus frame (ascend) from the depth stack (R4.7). */
  onAscend?: () => void
  /**
   * A non-jargon notice to surface when a descent could not open the linked
   * detail (cold link that failed/timed out — R4.5/R12). Null/absent = no notice.
   */
  descentNotice?: string | null
  /** Dismiss the descent notice. */
  onDismissNotice?: () => void
}) {
  const grouped = groupByRegion(workspace)
  // The environment stage shows once Neo's work has placed a non-narration
  // surface OR there is a live work centerpiece to host (the relocated Neo's
  // Computer). Until then the narration panel fills the environment body — but
  // the ENVIRONMENT remains the root either way (R3.5): chat never becomes the
  // page.
  const hasRegions = ENV_REGIONS.some((region) => grouped[region].length > 0)
  const hasEnvironment = hasRegions || !!environment

  // Mobile layout: the two-pane split (chat | stage) does not fit a phone, so
  // there the environment stage becomes a FULL-SCREEN overlay toggled by the
  // user — chat stays the primary surface and a floating control reveals Neo's
  // work when there is any. On lg+ the stage is an in-flow column beside the
  // chat (no overlay, no toggle). `stageOpen` only affects the <lg overlay.
  const [stageOpen, setStageOpen] = useState(false)
  // Desktop (lg+): the stage is closeable too. Some users want Neo's work in
  // view the whole time; others want the conversation full-width and only peek
  // at the work on demand. Closing is NOT an abort — the run keeps going; this
  // only hides the viewport. Independent of `stageOpen`, which is the <lg
  // full-screen overlay toggle.
  //
  // Two explicit intents are tracked rather than one open/closed flag, because
  // the DEFAULT depends on whether there is anything to look at: with no work
  // the stage starts closed (the app must not open onto an empty viewport), and
  // it opens itself once work arrives — unless the user has said otherwise, in
  // which case their choice wins for the rest of the run.
  const [userClosed, setUserClosed] = useState(false)
  const [userOpened, setUserOpened] = useState(false)
  const hasWork = hasRegions || !!environmentActive
  const desktopClosed = userClosed || (!hasWork && !userOpened)

  // When the environment empties (work cleared / new chat), drop the phone
  // overlay and both explicit intents so a stale choice never leaves the user
  // stuck on a blank stage or unable to see the next run's work.
  useEffect(() => {
    if (!hasEnvironment) {
      setStageOpen(false)
      setUserClosed(false)
      setUserOpened(false)
    }
  }, [hasEnvironment])

  // Only the TRANSITION into having work clears a previous run's explicit
  // close — so the next thing Neo does is visible rather than hidden behind a
  // choice made about older work, while a close made DURING a run keeps
  // sticking as that run's events keep arriving.
  const hadWork = useRef(hasWork)
  useEffect(() => {
    if (hasWork && !hadWork.current) setUserClosed(false)
    hadWork.current = hasWork
  }, [hasWork])

  return (
    <section
      id="main-content"
      data-shell-root
      data-environment-root
      className="bg-background relative flex h-svh w-full overflow-hidden"
    >
      {/* Narration (chat) — ONE region within the environment, never the page.
          Bounded to a column only while the stage is showing beside it on lg+;
          when the user closes the stage on desktop, chat expands to full width. */}
      <NarrationPanel bounded={hasEnvironment && !desktopClosed}>{narration}</NarrationPanel>

      {/* Environment stage — the home of every other region. Separated from the
          narration panel by background tone only (no border strokes). */}
      {hasEnvironment && (
        <motion.div
          layout
          initial={false}
          animate={stageOpen || !desktopClosed ? { opacity: 1, x: 0 } : { opacity: 0, x: 24 }}
          transition={{
            layout: motionTransition.layout,
            opacity: motionTransition.content,
            x: motionTransition.page,
          }}
          data-environment-stage
          data-desktop-closed={desktopClosed ? '' : undefined}
          className={cn(
            'bg-card relative flex min-w-0 flex-col gap-3 overflow-y-auto p-3',
            // lg+: in-flow column beside the chat, hidden when the user closes it
            // on desktop. <lg: full-bleed overlay, visible only when opened (chat
            // stays primary on a phone).
            desktopClosed
              ? 'lg:pointer-events-none lg:w-0 lg:flex-none lg:overflow-hidden lg:p-0'
              : 'lg:flex-1',
            'max-lg:fixed max-lg:inset-0 max-lg:z-40',
            !stageOpen && 'max-lg:hidden',
          )}
        >
          {/* Mobile-only: close the overlay and return to the conversation.
              Separation is by tone (a translucent scrim chip), never a stroke. */}
          <Button
            label="Close workspace"
            variant="secondary"
            size="sm"
            icon={<X className="size-5" />}
            isIconOnly
            onClick={() => setStageOpen(false)}
            className="absolute top-3 right-3 z-10 lg:hidden"
          />

          {/* Desktop-only: close the stage and give the conversation the full
              width. Closing is NOT an abort — the run keeps going; the reopen
              chip below brings the viewport back. */}
          <Button
            label="Close Neo's work"
            variant="secondary"
            size="sm"
            icon={<X className="size-[1.05rem]" />}
            isIconOnly
            onClick={() => {
              setUserClosed(true)
              setUserOpened(false)
            }}
            tooltip="Close"
            className="absolute top-3 right-3 z-10 hidden lg:flex"
          />

          {ENV_REGIONS.map((region) => {
            const placed = grouped[region]
            if (placed.length === 0) return null
            // The `activity` region hosts the live Timeline of Neo's activity
            // with its own glanceable-awareness chrome, rendered through the
            // reused renderer (task 6.2). Tapping a step descends into its
            // linked Stream at raw (task 6.3). Every other region keeps the
            // generic pass-through container until its own task fills it in.
            if (region === 'activity') {
              return (
                <ActivityRegion
                  key={region}
                  surfaces={placed}
                  handlers={handlers}
                  onDescendStep={onDescendStep}
                />
              )
            }
            return (
              <div key={region} data-region={region} className="flex flex-col gap-3">
                {placed.map((p) => (
                  <SurfaceRenderer key={p.surface.id} surface={p.surface} handlers={handlers} />
                ))}
              </div>
            )
          })}

          {/* Neo's live work centerpiece — the relocated "Neo's Computer".
              Renders the legacy NeoStep work (terminal / browser / sources /
              agents / media) that is not yet a typed Construct surface, big on
              the stage instead of squished beside the chat. Grows to fill the
              stage so it reads as the centerpiece, not a footnote. */}
          {environment && (
            <div data-region="work" className="flex min-h-[28rem] min-w-0 flex-1 flex-col">
              {environment}
            </div>
          )}

          {/* Depth navigation: the focused surface drawn at raw through the
              reused renderer, layered over the stage (R4.9, R17.3). Renders
              nothing when the depth stack is at the base. */}
          <RawFocusOverlay workspace={workspace} onAscend={onAscend} handlers={handlers} />
        </motion.div>
      )}

      {/* Mobile-only: reveal Neo's work. The two-pane stage can't sit beside the
          chat on a phone, so a floating control opens it as a full-screen
          overlay. Anchored at the TOP-right (below the chat header) — never the
          bottom — so it can never sit over the docked composer, whose send/attach
          controls span the full bottom edge. Hidden on lg+ (the stage is always
          in view there) and when the overlay is already open. Solid accent fill —
          no border, no glow. */}
      {hasEnvironment && !stageOpen && (
        <Button
          label="Open workspace"
          variant="primary"
          size="lg"
          icon={<Monitor className="size-4" />}
          onClick={() => setStageOpen(true)}
          className="absolute top-16 right-4 z-30 lg:hidden"
        />
      )}

      {/* Desktop-only: reopen the stage the user closed. Solid accent fill —
          tone only, no border, no glow. Hidden below lg (the mobile chip above
          governs there) and whenever the stage is open. */}
      {hasEnvironment && desktopClosed && (
        <Button
          label="Open Neo's work"
          variant="primary"
          size="lg"
          icon={<Monitor className="size-4" />}
          onClick={() => {
            setUserOpened(true)
            setUserClosed(false)
          }}
          className="absolute top-4 right-4 z-30 hidden lg:flex"
        />
      )}

      {/* A descent that could not open the linked detail (cold link that failed
          or timed out) is reported in NON-JARGON language; the focus stack was
          left unchanged (R4.5/R12). Elevation above the stage is by background
          TONE only (popover sits above card) — no border stroke, no shadow,
          no glow (R13.1/R13.2/R13.5). */}
      {descentNotice && (
        <div
          role="status"
          data-shell-notice
          className="bg-popover text-popover-foreground absolute bottom-3 left-1/2 z-30 flex -translate-x-1/2 items-center gap-3 rounded-xl px-4 py-2.5 text-sm"
        >
          <span>{descentNotice}</span>
          {onDismissNotice && (
            <Button label="Dismiss" variant="ghost" size="sm" onClick={onDismissNotice} />
          )}
        </div>
      )}
    </section>
  )
}
