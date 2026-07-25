'use client'

/**
 * NeoWorkbench — the ONE workbench (NEO-WORKBENCH req 2). A panel that slides
 * in from the right when Neo starts producing work (or via the manual
 * toggle), full-width under 1024px, with a three-position Code / Diff /
 * Preview slider over panels that STAY MOUNTED (they slide horizontally — no
 * unmount flash, so open files, scroll positions and diff selections survive
 * view switches), plus a toggleable terminal drawer.
 *
 * Shaped after bolt.diy's Workbench.client.tsx + ui/Slider.tsx, on our
 * tokens: separation is background-tone only (bg-surface-*), single accent,
 * no border strokes, no glow.
 */
import { type ReactNode } from 'react'

export type WorkbenchView = 'code' | 'diff' | 'preview'

const VIEWS: { id: WorkbenchView; label: string }[] = [
  { id: 'code', label: 'Code' },
  { id: 'diff', label: 'Diff' },
  { id: 'preview', label: 'Preview' },
]

/** The slider control: three positions, active = raised surface tone. */
export function WorkbenchSlider({
  view,
  onViewChange,
  previewReady,
}: {
  view: WorkbenchView
  onViewChange: (v: WorkbenchView) => void
  previewReady?: boolean
}) {
  return (
    <div
      role="tablist"
      aria-label="Workbench view"
      className="bg-surface-primary-contrast flex shrink-0 items-center gap-0.5 rounded-full p-0.5"
    >
      {VIEWS.map((v) => (
        <button
          key={v.id}
          role="tab"
          aria-selected={view === v.id}
          data-view={v.id}
          onClick={() => onViewChange(v.id)}
          className={
            view === v.id
              ? 'bg-surface-hover-alt text-foreground min-h-11 rounded-full px-2 text-xs font-medium sm:px-3'
              : 'text-muted-foreground hover:text-foreground min-h-11 rounded-full px-2 text-xs sm:px-3'
          }
        >
          <span className="flex items-center gap-1.5">
            {v.label}
            {v.id === 'preview' && previewReady ? (
              <span aria-label="preview ready" className="size-1.5 rounded-full bg-[var(--ring)]" />
            ) : null}
          </span>
        </button>
      ))}
    </div>
  )
}

export function NeoWorkbench({
  open,
  view,
  onViewChange,
  onClose,
  terminalOpen,
  onToggleTerminal,
  previewReady,
  code,
  diff,
  preview,
  terminal,
  header,
}: {
  open: boolean
  view: WorkbenchView
  onViewChange: (v: WorkbenchView) => void
  onClose: () => void
  terminalOpen: boolean
  onToggleTerminal: () => void
  previewReady?: boolean
  code: ReactNode
  diff: ReactNode
  preview: ReactNode
  terminal: ReactNode
  /** Extra header content (e.g. the file-changes dropdown), right-aligned. */
  header?: ReactNode
}) {
  const idx = VIEWS.findIndex((v) => v.id === view)
  return (
    <section
      aria-label="Workbench"
      data-testid="workbench"
      data-open={open}
      aria-hidden={!open}
      className={
        // The slide: width collapses on desktop; under 1024px the panel is a
        // full-width overlay translated off-screen when closed.
        'bg-surface-secondary-alt absolute inset-0 z-20 flex h-full min-h-0 w-full flex-col overflow-hidden ' +
        'transition-all duration-300 ease-out lg:relative lg:inset-auto lg:z-auto lg:rounded-l-xl' +
        (open
          ? ' translate-x-0 lg:w-[52%]'
          : ' pointer-events-none invisible translate-x-full lg:visible lg:w-0 lg:translate-x-0')
      }
    >
      <div className="flex h-12 shrink-0 items-center gap-1 px-2 sm:gap-2 sm:px-3">
        <WorkbenchSlider view={view} onViewChange={onViewChange} previewReady={previewReady} />
        <div className="ml-auto flex items-center gap-1">
          {header}
          <button
            aria-pressed={terminalOpen}
            onClick={onToggleTerminal}
            className={
              terminalOpen
                ? 'bg-surface-hover text-foreground min-h-11 rounded-md px-3 font-mono text-xs'
                : 'text-muted-foreground hover:text-foreground min-h-11 rounded-md px-3 font-mono text-xs'
            }
          >
            <span className="hidden min-[420px]:inline">Terminal</span>
            <span className="min-[420px]:hidden">Term</span>
          </button>
          <button
            aria-label="Close workbench"
            onClick={onClose}
            className="text-muted-foreground hover:text-foreground min-h-11 rounded-md px-3 text-xs"
          >
            Close
          </button>
        </div>
      </div>

      {/* The mounted panel strip: all three views live side by side and the
          strip translates by view index — bolt's slider idiom, no unmount. */}
      <div className="min-h-0 flex-1 overflow-hidden">
        <div
          data-testid="workbench-strip"
          className="flex h-full w-[300%] transition-transform duration-300 ease-out"
          style={{ transform: `translateX(-${(idx * 100) / 3}%)` }}
        >
          <div className="h-full min-h-0 w-1/3 overflow-hidden" data-panel="code">
            {code}
          </div>
          <div className="h-full min-h-0 w-1/3 overflow-hidden" data-panel="diff">
            {diff}
          </div>
          <div className="h-full min-h-0 w-1/3 overflow-hidden" data-panel="preview">
            {preview}
          </div>
        </div>
      </div>

      {/* Terminal drawer: stays mounted so scrollback survives the toggle. */}
      <div
        data-testid="workbench-terminal"
        data-open={terminalOpen}
        className={
          'shrink-0 overflow-hidden transition-all duration-200 ' + (terminalOpen ? 'h-56' : 'h-0')
        }
      >
        {terminal}
      </div>
    </section>
  )
}
