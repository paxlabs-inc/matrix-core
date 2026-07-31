'use client'

/**
 * NeoSelfModel — the agent's structural self-knowledge as a living organ.
 *
 * Renders the full codegraph (8k+ symbols across neo / cody / cortex /
 * executor) as a 3D brain: modules are anatomical regions, packages are
 * cortical clusters, and every symbol is a clickable neuron. Data comes from
 * the static /self-model/graph.json compiled straight out of
 * /root/matrix/graph/self-model by scripts/build-self-model-graph.mjs —
 * no daemon API involved.
 *
 * Design system: separation by background TONE only (bg-card / bg-muted),
 * single accent via text-primary, no emojis.
 */
import { useCallback, useEffect, useState } from 'react'
import dynamic from 'next/dynamic'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { Dialog } from '@astryxdesign/core/Dialog'
import { Button } from '@astryxdesign/core/Button'
import {
  AlertTriangle,
  BrainIcon,
  ChevronRight,
  Code,
  Cpu,
  FileText,
  Workflow,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import {
  loadSelfModelGraph,
  nodePath,
  MODULE_COLORS,
  type SelfModelGraph,
} from './self-model-graph'

const NeoBrain3D = dynamic(() => import('./neo-brain-3d'), {
  ssr: false,
  loading: () => (
    <div className="absolute inset-0 grid place-items-center">
      <PulsingBrain label="Initializing WebGL…" />
    </div>
  ),
})

/* ------------------------------------------------------------------ */
/* Small pieces                                                        */
/* ------------------------------------------------------------------ */

function PulsingBrain({ label }: { label: string }) {
  return (
    <div className="flex flex-col items-center gap-3 text-center">
      <motion.div
        animate={{ scale: [1, 1.12, 1], opacity: [0.5, 1, 0.5] }}
        transition={{ duration: 2.2, repeat: Infinity, ease: 'easeInOut' }}
      >
        <BrainIcon className="text-primary size-8" />
      </motion.div>
      <p className="text-muted-foreground/70 text-xs">{label}</p>
    </div>
  )
}

const KIND_ICONS: Record<string, typeof Cpu> = {
  package: Workflow,
  file: FileText,
  type: Code,
  interface: Code,
  method: Cpu,
  func: Cpu,
}

function KindIcon({ kind, className }: { kind: string; className?: string }) {
  const Icon = KIND_ICONS[kind] ?? Cpu
  return <Icon className={className} />
}

function moduleColor(graph: SelfModelGraph, m: number): string {
  const id = graph.meta.modules[m]?.id ?? ''
  return MODULE_COLORS[id] ?? '#9aa4b2'
}

/* ------------------------------------------------------------------ */
/* Node detail panel                                                   */
/* ------------------------------------------------------------------ */

function NodeDetail({
  graph,
  idx,
  onNavigate,
  onClose,
}: {
  graph: SelfModelGraph
  idx: number
  onNavigate: (idx: number) => void
  onClose: () => void
}) {
  const node = graph.nodes[idx]
  const color = moduleColor(graph, node.m)
  const path = nodePath(graph, idx)
  const children = node.c ?? []
  const impl = graph.impl.filter(([a, b]) => a === idx || b === idx)
  const [showAllChildren, setShowAllChildren] = useState(false)
  const visibleChildren = showAllChildren ? children : children.slice(0, 24)

  return (
    <motion.aside
      key={node.id}
      initial={{ opacity: 0, x: 24, y: 0 }}
      animate={{ opacity: 1, x: 0, y: 0 }}
      exit={{ opacity: 0, x: 24, y: 0 }}
      transition={{ duration: 0.22, ease: [0.32, 0.72, 0, 1] }}
      className="bg-card/85 pointer-events-auto flex max-h-[60vh] w-full flex-col overflow-hidden rounded-t-2xl backdrop-blur-xl sm:max-h-[calc(100%-1rem)] sm:w-80 sm:rounded-2xl lg:w-96"
    >
      <div className="h-0.5 shrink-0" style={{ background: color }} />

      {/* Header */}
      <div className="flex items-start gap-3 p-4 pb-3">
        <span
          className="grid size-9 shrink-0 place-items-center rounded-xl"
          style={{ background: `color-mix(in oklab, ${color} 14%, transparent)` }}
        >
          <KindIcon kind={node.k} className="size-[1.05rem]" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-foreground truncate font-mono text-sm font-bold tracking-tight">
            {node.n}
          </p>
          <div className="mt-0.5 flex items-center gap-2">
            <span
              className="rounded px-1.5 py-0.5 font-mono text-[0.58rem] font-medium tracking-wide uppercase"
              style={{
                background: `color-mix(in oklab, ${color} 14%, transparent)`,
                color,
              }}
            >
              {node.k}
            </span>
            <span className="text-muted-foreground/60 font-mono text-[0.58rem]">
              {graph.meta.modules[node.m]?.id}
            </span>
          </div>
        </div>
        <button
          type="button"
          onClick={onClose}
          aria-label="Deselect node"
          className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-7 shrink-0 place-items-center rounded-full transition"
        >
          <X className="size-3.5" />
        </button>
      </div>

      {/* Body */}
      <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
        {/* Breadcrumb */}
        {path.length > 1 && (
          <div className="mb-3 flex flex-wrap items-center gap-0.5">
            {path.map((p, i) => (
              <span key={graph.nodes[p].id} className="flex items-center gap-0.5">
                {i > 0 && <ChevronRight className="text-muted-foreground/40 size-2.5" />}
                <button
                  type="button"
                  onClick={() => onNavigate(p)}
                  disabled={p === idx}
                  className={cn(
                    'rounded px-1 py-0.5 font-mono text-[0.6rem] transition',
                    p === idx
                      ? 'text-foreground font-semibold'
                      : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
                  )}
                >
                  {graph.nodes[p].n}
                </button>
              </span>
            ))}
          </div>
        )}

        <div className="flex flex-col gap-3">
          {/* Location */}
          <div>
            <p className="text-muted-foreground/70 mb-0.5 text-[0.58rem] font-semibold tracking-widest uppercase">
              Location
            </p>
            <p className="text-foreground/85 font-mono text-[0.7rem] leading-snug break-all">
              {node.f}
              {node.l && node.l !== '0:0' && (
                <span className="text-muted-foreground">:{node.l.replace(':', '–')}</span>
              )}
            </p>
          </div>

          {/* Signature */}
          {node.s && (
            <div>
              <p className="text-muted-foreground/70 mb-0.5 text-[0.58rem] font-semibold tracking-widest uppercase">
                Signature
              </p>
              <pre className="bg-muted/40 text-foreground/85 max-h-44 overflow-y-auto rounded-lg p-2.5 font-mono text-[0.66rem] leading-relaxed break-all whitespace-pre-wrap">
                {node.s}
              </pre>
            </div>
          )}

          {/* Doc */}
          {node.d && (
            <div>
              <p className="text-muted-foreground/70 mb-0.5 text-[0.58rem] font-semibold tracking-widest uppercase">
                Doc
              </p>
              <p className="text-muted-foreground text-[0.72rem] leading-relaxed whitespace-pre-line">
                {node.d}
              </p>
            </div>
          )}

          {/* Implements */}
          {impl.length > 0 && (
            <div>
              <p className="text-muted-foreground/70 mb-1 text-[0.58rem] font-semibold tracking-widest uppercase">
                Implements
              </p>
              <div className="flex flex-wrap gap-1">
                {impl.map(([a, b]) => {
                  const other = a === idx ? b : a
                  return (
                    <button
                      key={`${a}-${b}`}
                      type="button"
                      onClick={() => onNavigate(other)}
                      className="bg-muted/50 text-foreground/80 hover:bg-muted rounded-md px-2 py-1 font-mono text-[0.62rem] transition"
                    >
                      {graph.nodes[other].n}
                    </button>
                  )
                })}
              </div>
            </div>
          )}

          {/* Children */}
          {children.length > 0 && (
            <div>
              <p className="text-muted-foreground/70 mb-1 text-[0.58rem] font-semibold tracking-widest uppercase">
                Contains ({children.length})
              </p>
              <div className="flex flex-wrap gap-1">
                {visibleChildren.map((c) => (
                  <button
                    key={graph.nodes[c].id}
                    type="button"
                    onClick={() => onNavigate(c)}
                    className="bg-muted/50 text-foreground/80 hover:bg-muted flex items-center gap-1 rounded-md px-2 py-1 font-mono text-[0.62rem] transition"
                  >
                    <KindIcon kind={graph.nodes[c].k} className="text-muted-foreground size-2.5" />
                    {graph.nodes[c].n}
                  </button>
                ))}
                {children.length > visibleChildren.length && (
                  <button
                    type="button"
                    onClick={() => setShowAllChildren(true)}
                    className="text-primary rounded-md px-2 py-1 font-mono text-[0.62rem]"
                  >
                    +{children.length - visibleChildren.length} more
                  </button>
                )}
              </div>
            </div>
          )}
        </div>
      </div>
    </motion.aside>
  )
}

/* ------------------------------------------------------------------ */
/* Module legend                                                       */
/* ------------------------------------------------------------------ */

const REGION_LABELS: Record<string, string> = {
  neo: 'left hemisphere — conversation',
  cody: 'right hemisphere — coding',
  cortex: 'limbic core — memory',
  executor: 'cerebellum — execution',
}

function ModuleLegend({
  graph,
  filter,
  onFilter,
}: {
  graph: SelfModelGraph
  filter: number
  onFilter: (m: number) => void
}) {
  return (
    <div className="pointer-events-auto flex flex-row flex-wrap gap-1 sm:flex-col sm:gap-1">
      {graph.meta.modules.map((mod, i) => {
        const color = MODULE_COLORS[mod.id] ?? '#9aa4b2'
        const active = filter === -1 || filter === i
        return (
          <button
            key={mod.id}
            type="button"
            onClick={() => onFilter(filter === i ? -1 : i)}
            className={cn(
              'bg-card/70 flex items-center gap-1.5 rounded-lg px-2 py-1 backdrop-blur-md transition sm:gap-2 sm:px-2.5 sm:py-1.5',
              active ? 'opacity-100' : 'opacity-40 hover:opacity-70',
            )}
          >
            <span className="size-2 shrink-0 rounded-full" style={{ background: color }} />
            <span className="text-foreground font-mono text-[0.62rem] font-semibold sm:text-[0.68rem]">
              {mod.id}
            </span>
            <span className="text-muted-foreground/60 text-[0.55rem] sm:text-[0.58rem]">
              {mod.count.toLocaleString()}
            </span>
            <span className="text-muted-foreground/50 hidden text-[0.55rem] sm:inline">
              {REGION_LABELS[mod.id] ?? ''}
            </span>
          </button>
        )
      })}
    </div>
  )
}

/* ------------------------------------------------------------------ */
/* Main component                                                      */
/* ------------------------------------------------------------------ */

export function NeoSelfModel() {
  const [graph, setGraph] = useState<SelfModelGraph | null>(null)
  const [loading, setLoading] = useState(true)
  const [selected, setSelected] = useState(-1)
  const [moduleFilter, setModuleFilter] = useState(-1)
  const reducedMotion = useReducedMotion() ?? false

  useEffect(() => {
    const ctrl = new AbortController()
    loadSelfModelGraph(ctrl.signal)
      .then((g) => setGraph(g))
      .finally(() => setLoading(false))
    return () => ctrl.abort()
  }, [])

  const handleSelect = useCallback((idx: number) => setSelected(idx), [])

  if (loading) {
    return (
      <div className="absolute inset-0 grid place-items-center">
        <PulsingBrain label="Loading self-model graph…" />
      </div>
    )
  }

  if (!graph) {
    return (
      <div className="text-muted-foreground/60 absolute inset-0 grid place-items-center">
        <div className="flex flex-col items-center gap-2 text-center">
          <AlertTriangle className="size-6" />
          <p className="text-xs">Self-model graph unavailable</p>
          <p className="text-muted-foreground/40 max-w-56 text-[0.62rem]">
            Run pnpm build:selfmodel to compile /root/matrix/graph/self-model into
            public/self-model/graph.json
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="absolute inset-0 overflow-hidden" style={{ background: '#05070d' }}>
      {/* 3D brain */}
      <NeoBrain3D
        graph={graph}
        selected={selected}
        onSelect={handleSelect}
        moduleFilter={moduleFilter}
        reducedMotion={reducedMotion}
      />

      {/* HUD — top left: identity + stats */}
      <div className="pointer-events-none absolute top-2 left-2 flex flex-col gap-1.5 sm:top-3 sm:left-3 sm:gap-2">
        <div className="bg-card/70 pointer-events-auto flex items-center gap-2 rounded-xl px-2.5 py-1.5 backdrop-blur-md sm:gap-2.5 sm:px-3 sm:py-2">
          <span className="bg-primary/15 text-primary grid size-7 shrink-0 place-items-center rounded-lg sm:size-8">
            <BrainIcon className="size-3.5 sm:size-4" />
          </span>
          <div className="min-w-0">
            <p className="text-foreground text-[0.68rem] font-bold tracking-tight sm:text-xs">
              Neo — self-model
            </p>
            <p className="text-muted-foreground/70 font-mono text-[0.52rem] sm:text-[0.58rem]">
              {graph.nodes.length.toLocaleString()} symbols · merkle{' '}
              {graph.meta.merkle.slice(3, 11)}
            </p>
          </div>
        </div>
        <ModuleLegend graph={graph} filter={moduleFilter} onFilter={setModuleFilter} />
      </div>

      {/* HUD — bottom left: hint (hidden on mobile when detail panel is open) */}
      <p className="text-muted-foreground/40 pointer-events-none absolute bottom-2 left-2 text-[0.52rem] sm:bottom-3 sm:left-3 sm:text-[0.6rem]">
        <span className="hidden sm:inline">
          drag to orbit · scroll to zoom · click a neuron to inspect
        </span>
        <span className="sm:hidden">tap to inspect · pinch to zoom</span>
      </p>

      {/* Detail panel — right sidebar on desktop, bottom sheet on mobile */}
      <div className="pointer-events-none absolute inset-x-0 bottom-0 z-20 flex justify-center sm:inset-x-auto sm:top-3 sm:right-3 sm:bottom-3 sm:items-start sm:justify-end sm:pt-14">
        <AnimatePresence mode="wait">
          {selected >= 0 && graph.nodes[selected] && (
            <NodeDetail
              graph={graph}
              idx={selected}
              onNavigate={handleSelect}
              onClose={() => setSelected(-1)}
            />
          )}
        </AnimatePresence>
      </div>
    </div>
  )
}

const EASE = [0.32, 0.72, 0, 1] as const

/** Full-page overlay wrapper. */
export function NeoSelfModelOverlay({ open, onClose }: { open: boolean; onClose: () => void }) {
  const reduce = useReducedMotion()

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  return (
    <Dialog
      isOpen={open}
      onOpenChange={(next) => {
        if (!next) onClose()
      }}
      variant="fullscreen"
      purpose="info"
      padding={0}
      aria-label="Neo self model"
    >
      <motion.div
        initial={reduce ? false : { opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        transition={reduce ? { duration: 0 } : { duration: 0.25, ease: EASE }}
        className="flex h-dvh flex-col overflow-hidden"
        style={{ background: '#05070d' }}
      >
        {/* Close */}
        <Button
          label="Close self model"
          variant="secondary"
          size="sm"
          icon={<X className="size-[0.95rem] sm:size-[1.05rem]" />}
          isIconOnly
          onClick={onClose}
          className="absolute right-2 bottom-2 z-30 sm:right-3 sm:bottom-3"
        />

        {/* Body */}
        <div className="relative min-h-0 flex-1">
          <NeoSelfModel />
        </div>
      </motion.div>
    </Dialog>
  )
}
