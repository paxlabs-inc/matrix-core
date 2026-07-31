'use client'

/**
 * SurfaceRenderer — the trusted dispatcher.
 *
 * Reads `surface.kind` and renders the matching primitive from the fixed set
 * of 8 (invariant i2: the agent fills trusted primitives, NEVER emits arbitrary
 * UI — an unknown kind degrades to nothing, never to executed markup). The
 * attribute block decorates above the primitive (stakes/cost/confidence), and
 * `stakes=irreversible` is threaded into the Entity so a committed act reads
 * with the heavier tone. `ConstructSurfaces` is the animated list container the
 * workspace mounts.
 */
import { AnimatePresence, motion } from 'motion/react'
import type { Surface, AskResponse } from '@/lib/construct/types.gen'
import { DecorationRow, hasDecoration } from '@/components/matrix/construct/decoration'
import { NarrationView } from '@/components/matrix/construct/narration'
import { MetricView } from '@/components/matrix/construct/metric'
import { EntityView } from '@/components/matrix/construct/entity'
import { StructureView } from '@/components/matrix/construct/structure'
import { StreamView } from '@/components/matrix/construct/stream'
import { TimelineView } from '@/components/matrix/construct/timeline'
import { CanvasView } from '@/components/matrix/construct/canvas'
import { AskView } from '@/components/matrix/construct/ask'

const EASE = [0.32, 0.72, 0, 1] as const

export interface SurfaceHandlers {
  /** Fire an Ask referenced by an Entity affordance / Canvas region. */
  onAsk?: (askRef: string) => void
  /** Submit a typed Ask response (the back-channel; wired in Phase 5). */
  onRespond?: (surfaceId: string, response: AskResponse) => void
}

function Primitive({ surface, handlers }: { surface: Surface; handlers?: SurfaceHandlers }) {
  switch (surface.kind) {
    case 'narration':
      return surface.narration ? <NarrationView narration={surface.narration} /> : null
    case 'metric':
      return surface.metric ? <MetricView metric={surface.metric} /> : null
    case 'entity':
      return surface.entity ? (
        <EntityView
          entity={surface.entity}
          irreversible={surface.attributes?.stakes === 'irreversible'}
          onAsk={handlers?.onAsk}
        />
      ) : null
    case 'structure':
      return surface.structure ? <StructureView structure={surface.structure} /> : null
    case 'stream':
      return surface.stream ? <StreamView stream={surface.stream} /> : null
    case 'timeline':
      return surface.timeline ? <TimelineView timeline={surface.timeline} /> : null
    case 'canvas':
      return surface.canvas ? <CanvasView canvas={surface.canvas} onAsk={handlers?.onAsk} /> : null
    case 'ask':
      return surface.ask ? (
        <AskView id={surface.id} ask={surface.ask} onRespond={handlers?.onRespond} />
      ) : null
    default:
      // Unknown kind: render nothing. Safety from fixed renderers (i2).
      return null
  }
}

export function SurfaceRenderer({
  surface,
  handlers,
}: {
  surface: Surface
  handlers?: SurfaceHandlers
}) {
  const decorated = hasDecoration(surface.attributes)
  const body = <Primitive surface={surface} handlers={handlers} />
  if (!decorated) return body
  return (
    <div className="flex flex-col gap-1.5">
      <DecorationRow attributes={surface.attributes} />
      {body}
    </div>
  )
}

/**
 * ConstructSurfaces — the animated list the workspace renders. Each surface
 * enters with the same spring the rest of the surface uses; keyed by stable id
 * so a progressive patch updates in place rather than remounting.
 */
export function ConstructSurfaces({
  surfaces,
  handlers,
}: {
  surfaces: Surface[]
  handlers?: SurfaceHandlers
}) {
  if (surfaces.length === 0) return null
  return (
    <div className="flex flex-col gap-3">
      <AnimatePresence initial={false}>
        {surfaces.map((s) => (
          <motion.div
            key={s.id}
            layout
            initial={{ opacity: 0, y: 6 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.26, ease: EASE }}
          >
            <SurfaceRenderer surface={s} handlers={handlers} />
          </motion.div>
        ))}
      </AnimatePresence>
    </div>
  )
}
