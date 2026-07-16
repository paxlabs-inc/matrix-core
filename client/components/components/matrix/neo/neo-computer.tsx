'use client'

/**
 * NeoComputer — "Neo's Computer", the dedicated live work viewport.
 *
 * Promoted out of the inline WorkWindow into its OWN openable / closable
 * right-side panel (the Kimi-style two-pane workspace): the conversation rail
 * stays a clean chat thread, while everything Neo is actually DOING — the
 * terminal / browser / editor viewports, web-search source cards, the Agent
 * Swarm windows and generated media — renders here as a focused "screen".
 *
 * It is the show-the-work differentiator given room to breathe. The panel
 * auto-opens when Neo starts working and can be collapsed at any time (the
 * collapsed status chip in the surface reopens it). Closing the panel is NOT
 * an abort — the run keeps going; abort still lives on the composer.
 *
 * Fused to the client design system: separation by background TONE only (no
 * border strokes for depth), single accent #004ced via `--primary`, the same
 * "ready/success" green the surface uses, rounded brand font for chrome.
 */
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import {
  BrainIcon,
  CheckCircle,
  FileText,
  Globe,
  ImageIcon,
  Monitor,
  Radio,
  ShieldAlert,
  Sparkles,
  TerminalIcon,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { ChatPhase, NeoStep, NeoTask } from '@/hooks/api/useChat'
import type { AskResponse } from '@/lib/construct/types.gen'
import {
  NeoWorkspace,
  NeoThinkingStrip,
  type FilmstripFrame,
} from '@/components/matrix/neo/neo-workspace'
import { NeoSwarm } from '@/components/matrix/neo/neo-swarm'
import { NeoTodoList } from '@/components/matrix/neo/neo-todo'
import { NeoSearchResults } from '@/components/matrix/neo/neo-search'
import { NeoMediaGrid } from '@/components/matrix/neo/neo-media'
import { NeoArtifacts } from '@/components/matrix/neo/neo-artifacts'
import { NeoMemoryCard } from '@/components/matrix/neo/neo-memory'
import { NeoAuditTrail } from '@/components/matrix/neo/neo-audit'
import { ConstructSurfaces } from '@/components/matrix/construct'

const EASE = [0.32, 0.72, 0, 1] as const
const DONE_COLOR = 'oklch(0.72 0.14 155)' // the surface's "ready/success" green

/** The live status shown in the panel header (mirrors the surface StatePill). */
function statusFor(
  task: NeoTask,
  phase: ChatPhase,
): { label: string; color?: string; live: boolean } {
  if (phase === 'thinking') return { label: 'Thinking…', live: true }
  if (phase === 'working') return { label: 'Working…', live: true }
  if (task.failed) return { label: 'Stopped', color: 'oklch(0.62 0.2 25)', live: false }
  if (task.done) return { label: 'Done', color: DONE_COLOR, live: false }
  return { label: 'Working…', live: true }
}

type IconType = typeof Globe

/* ------------------------------------------------------------------ */
/* Screen model — Neo's Computer shows ONE screen at a time            */
/* ------------------------------------------------------------------ */

interface Screen {
  id: string
  label: string
  icon: IconType
  running: boolean
  node: ReactNode
}

function hostOf(url?: string): string {
  if (!url) return 'page'
  try {
    return new URL(url).host || url
  } catch {
    return url.replace(/^https?:\/\//, '').split('/')[0] || url
  }
}

function baseName(p?: string): string {
  if (!p) return 'file'
  const parts = p.split('/').filter(Boolean)
  return parts[parts.length - 1] || p
}

function stepLabel(step: NeoStep): string {
  if (step.kind === 'terminal') return 'Terminal'
  if (step.kind === 'browser') return hostOf(step.url)
  if (step.kind === 'editor') return baseName(step.path)
  return step.title || 'Step'
}

function stepIcon(step: NeoStep): IconType {
  if (step.kind === 'terminal') return TerminalIcon
  if (step.kind === 'editor') return FileText
  return Globe
}

function surfaceLabel(s: NeoTask['surfaces'][number]): string {
  const k: string = s.kind || 'view'
  return k.charAt(0).toUpperCase() + k.slice(1)
}

/**
 * Flatten the live task into an ordered list of single "screens": each
 * page-read / terminal / editor is its OWN screen, and sources / agents /
 * media each collapse into one. This is what lets the viewport switch from one
 * thing to the next — like a human flipping browser tabs — instead of stacking
 * every render and the terminal into one endless scroll.
 */
function buildScreens(
  task: NeoTask,
  phase: ChatPhase,
  showMedia: boolean,
  onRespond?: (surfaceId: string, response: AskResponse) => void,
  legacyOnly = false,
  onMediaAction?: (instruction: string) => void,
): Screen[] {
  const list: Screen[] = []
  const live = phase === 'thinking' || phase === 'working'

  // Neo's activated working memory (memory.activation): the "story so far" +
  // coarse timeline it is carrying forward. Read-only; leads the screens so the
  // user can always glance at what Neo remembers. Present in both the typed-
  // surface path and the legacy path.
  if (
    task.memory &&
    (task.memory.storySoFar || task.memory.timeline.length > 0 || task.memory.excerpts.length > 0)
  ) {
    list.push({
      id: 'memory',
      label: 'Memory',
      icon: BrainIcon,
      running: false,
      node: <NeoMemoryCard memory={task.memory} />,
    })
  }

  // Construct projection path: one screen per typed surface. Skipped when this
  // computer is the shell's centerpiece (legacyOnly): there the Construct
  // surfaces are rendered by the environment-stage feed, so this pane carries
  // ONLY the legacy NeoStep work (terminal / browser / sources / agents / media)
  // and never double-renders the typed surfaces.
  if (!legacyOnly && task.surfaces.length > 0) {
    task.surfaces.forEach((s, i) => {
      list.push({
        id: s.id || `surface-${i}`,
        label: surfaceLabel(s),
        icon: Monitor,
        running: live && i === task.surfaces.length - 1,
        node: <ConstructSurfaces surfaces={[s]} handlers={{ onRespond }} />,
      })
    })
    return list
  }

  // Live task-list (neo-smoothness req.3): the ordered checklist leads the
  // legacy path so the plan is the first screen the user can flip to. Running
  // while any item is still pending/in-progress.
  if (task.todos && task.todos.length > 0) {
    list.push({
      id: 'plan',
      label: 'Plan',
      icon: CheckCircle,
      running: live && task.todos.some((t) => t.status !== 'done'),
      node: <NeoTodoList todos={task.todos} />,
    })
  }

  // Legacy NeoStep path: each viewport step is a screen, in the order Neo
  // worked through them; the rich aggregates (sources / agents / media) follow.
  // The session's browsing stills, in capture order, feed every browser
  // screen's history strip (BROWSER-FILMSTRIP req.6 — click-to-scrub).
  const filmstrip: FilmstripFrame[] = task.steps
    .filter((s) => s.kind === 'browser' && !!s.screenshotUrl)
    .map((s) => ({ id: s.id, screenshotUrl: s.screenshotUrl!, pageTitle: s.pageTitle, ts: s.ts }))
  for (const st of task.steps) {
    if (st.kind === 'terminal' || st.kind === 'browser' || st.kind === 'editor') {
      list.push({
        id: st.id,
        label: stepLabel(st),
        icon: stepIcon(st),
        running: !!st.running,
        node: (
          <NeoWorkspace steps={[st]} filmstrip={st.kind === 'browser' ? filmstrip : undefined} />
        ),
      })
    }
  }
  if (task.searches.length > 0) {
    list.push({
      id: 'sources',
      label: 'Sources',
      icon: Globe,
      running: false,
      node: <NeoSearchResults searches={task.searches} />,
    })
  }
  if (task.swarm) {
    list.push({
      id: 'swarm',
      label: 'Agents',
      icon: Sparkles,
      running: live,
      node: <NeoSwarm swarm={task.swarm} />,
    })
  }
  if (showMedia && task.media.length > 0) {
    list.push({
      id: 'media',
      label: 'Media',
      icon: ImageIcon,
      running: false,
      node: <NeoMediaGrid media={task.media} onAction={onMediaAction} />,
    })
  }
  // F6: deliverable artifacts (downloadable files/archives, deployed sites).
  if (task.artifacts.length > 0) {
    list.push({
      id: 'artifacts',
      label: 'Files',
      icon: FileText,
      running: false,
      node: <NeoArtifacts artifacts={task.artifacts} />,
    })
  }
  // Cassandra 2.0 audit trail: silent-voice controller edits + identity leaks.
  if (task.audit && task.audit.length > 0) {
    list.push({
      id: 'audit',
      label: 'Audit',
      icon: ShieldAlert,
      running: false,
      node: <NeoAuditTrail audit={task.audit} />,
    })
  }
  return list
}

export function NeoComputer({
  task,
  phase,
  reduce,
  showMedia,
  onRespond,
  onMediaAction,
  onClose,
  legacyOnly = false,
  className,
}: {
  task: NeoTask
  phase: ChatPhase
  reduce: boolean
  /** Suppress live media once the closing turn carries it, to avoid doubling. */
  showMedia: boolean
  /** Answer a parked Construct Ask (the bidirectional back-channel). */
  onRespond?: (surfaceId: string, response: AskResponse) => void
  /** Post-generation image actions (tweak / variations / suggestions) → Neo. */
  onMediaAction?: (instruction: string) => void
  /** Collapse the panel (NOT an abort — the run keeps going). Omitted when this
   *  is the shell's fixed centerpiece, which is not collapsible. */
  onClose?: () => void
  /** Render only the legacy NeoStep work, leaving the typed Construct surfaces
   *  to the shell's environment-stage feed (the centerpiece use). */
  legacyOnly?: boolean
  /** Responsive placement / sizing supplied by the surface (in-flow column on
   *  lg+, full-bleed overlay drawer below it). */
  className?: string
}) {
  const status = statusFor(task, phase)
  const screens = buildScreens(task, phase, showMedia, onRespond, legacyOnly, onMediaAction)

  // Single-viewport navigation. By default Neo "drives" — the newest screen is
  // shown as work streams in. The user can click a tab to inspect an earlier
  // screen (that choice sticks, so we stop auto-following), and a "Live" pill
  // jumps back to whatever Neo is doing now.
  const [manual, setManual] = useState<string | null>(null)
  const lastId = screens.length > 0 ? screens[screens.length - 1].id : null
  const manualIndex = manual ? screens.findIndex((s) => s.id === manual) : -1
  const activeIndex = manualIndex >= 0 ? manualIndex : screens.length - 1
  const active = activeIndex >= 0 ? screens[activeIndex] : undefined
  const following = manualIndex < 0
  const HeroIcon = active?.icon ?? (task.searches.length > 0 ? Globe : Monitor)

  const select = (id: string) => setManual(id === lastId ? null : id)

  // Each screen opens at its top; switching tabs resets the scroll position.
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = bodyRef.current
    if (el) el.scrollTop = 0
  }, [active?.id])

  return (
    <motion.div
      initial={reduce ? false : { opacity: 0, x: 28 }}
      animate={{ opacity: 1, x: 0 }}
      exit={reduce ? { opacity: 0 } : { opacity: 0, x: 28 }}
      transition={reduce ? { duration: 0 } : { duration: 0.34, ease: EASE }}
      className={cn(
        'bg-card flex h-full min-h-0 w-full flex-col overflow-hidden rounded-2xl',
        className,
      )}
    >
      {/* header — the "computer" chrome: title + live status + collapse */}
      <div className="flex shrink-0 items-center gap-3 px-4 py-3.5">
        <span className="bg-primary/15 text-primary grid size-8 shrink-0 place-items-center rounded-[0.7rem]">
          <HeroIcon className="size-[1.05rem]" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-foreground truncate text-[0.9rem] leading-tight font-bold">
            Neo&apos;s Computer
          </p>
          <p className="text-muted-foreground mt-0.5 flex items-center gap-1.5 font-mono text-[0.7rem]">
            <span
              className={cn('size-1.5 rounded-full', status.live && 'bg-primary animate-pulse')}
              style={status.color ? { background: status.color } : undefined}
            />
            {status.label}
          </p>
        </div>
        {onClose && (
          <button
            type="button"
            onClick={onClose}
            aria-label="Collapse Neo's Computer"
            title="Collapse"
            className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-8 shrink-0 place-items-center rounded-full transition"
          >
            <X className="size-[1.05rem]" />
          </button>
        )}
      </div>

      {/* Neo's running narration — global to the run, above the screen tabs */}
      {task.thinking ? (
        <div className="shrink-0 px-4 pb-2">
          <NeoThinkingStrip text={task.thinking} />
        </div>
      ) : null}

      {/* tab strip — the open "screens"; one is shown at a time below. Only
          surfaced once there is more than one screen to switch between. */}
      {screens.length > 1 ? (
        <div className="flex shrink-0 items-center gap-1.5 px-3 pb-2">
          <div className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
            {screens.map((s) => {
              const Icon = s.icon
              const on = s.id === active?.id
              return (
                <button
                  key={s.id}
                  type="button"
                  onClick={() => select(s.id)}
                  title={s.label}
                  className={cn(
                    'flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors',
                    on ? 'bg-muted text-foreground' : 'text-muted-foreground hover:bg-muted/50',
                  )}
                >
                  <Icon className="size-3.5 shrink-0" />
                  <span className="max-w-[9rem] truncate">{s.label}</span>
                  {s.running ? (
                    <span className="bg-primary size-1.5 shrink-0 animate-pulse rounded-full" />
                  ) : null}
                </button>
              )
            })}
          </div>
          {!following ? (
            <button
              type="button"
              onClick={() => setManual(null)}
              title="Follow what Neo is doing now"
              className="text-primary hover:bg-primary/10 flex shrink-0 items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors"
            >
              <Radio className="size-3.5" /> Live
            </button>
          ) : null}
        </div>
      ) : null}

      {/* body — the single active screen */}
      <div ref={bodyRef} className="min-h-0 flex-1 overflow-x-hidden overflow-y-auto px-4 pb-4">
        {active ? (
          <AnimatePresence mode="wait">
            <motion.div
              key={active.id}
              initial={reduce ? false : { opacity: 0, y: 6 }}
              animate={{ opacity: 1, y: 0 }}
              exit={reduce ? { opacity: 0 } : { opacity: 0, y: -6 }}
              transition={reduce ? { duration: 0 } : { duration: 0.2, ease: EASE }}
            >
              {active.node}
            </motion.div>
          </AnimatePresence>
        ) : (
          <div className="text-muted-foreground/70 flex flex-col items-center gap-2 py-12 text-center">
            <Sparkles className={cn('size-6', status.live && 'animate-pulse')} />
            <p className="text-xs">Neo is getting to work…</p>
          </div>
        )}
      </div>
    </motion.div>
  )
}
