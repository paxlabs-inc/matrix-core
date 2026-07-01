/**
 * Legacy NeoStep -> Construct adapter (the WRAP-first bridge).
 *
 * Decision §1.5 of the Construct plan is pre-locked: wrap the closed NeoStep
 * union onto the open Construct primitives FIRST, then retire it — so the
 * client keeps rendering every existing run with zero regression while the new
 * trusted renderers take over.
 *
 * This is the projection the daemon will eventually do server-side (Phase 4);
 * here it runs client-side over the already-derived `NeoTask` so the new
 * `SurfaceRenderer` can paint today's live stream. Each NeoStep / aggregate
 * maps onto exactly one of the 8 frozen primitives:
 *
 *   narration  -> Narration            terminal -> Stream
 *   browser    -> Canvas(page/image)   editor   -> Entity(file)
 *   action     -> Entity(action)       search   -> Structure(list) + Narration
 *   media      -> Canvas(image/video)  swarm    -> Timeline
 *   answer     -> Narration(answer)
 *
 * Pure + deterministic: same task in -> same surfaces out (golden-testable).
 */
import type { NeoStep, NeoTask, ChatMedia, NeoSearch, NeoSwarm } from '@/hooks/api/useChat'
import type {
  Surface,
  StreamChunk,
  EntityField,
  Affordance,
  StructureNode,
  TimelineStep,
  StepStatus,
  MediaKind,
} from '@/lib/construct/types.gen'

function baseName(p?: string): string {
  if (!p) return 'untitled'
  const parts = p.split('/').filter(Boolean)
  return parts[parts.length - 1] || p
}

/** terminal -> Stream: the command is chunk 0, output is chunk 1. */
function terminalSurface(step: NeoStep): Surface {
  const chunks: StreamChunk[] = []
  if (step.command) chunks.push({ seq: 0, text: `$ ${step.command}`, channel: 'command' })
  if (step.output) chunks.push({ seq: 1, text: step.output, channel: 'stdout' })
  return {
    kind: 'stream',
    id: step.id,
    stream: {
      source: step.cwd || 'terminal',
      title: step.title || 'Terminal',
      chunks,
      closed: !step.running,
    },
  }
}

/** browser -> Canvas: a screenshot renders as an image, otherwise the page. */
function browserSurface(step: NeoStep): Surface {
  const kind: MediaKind = step.screenshotUrl ? 'image' : 'page'
  return {
    kind: 'canvas',
    id: step.id,
    canvas: {
      media: { kind, url: step.screenshotUrl || step.url, alt: step.pageTitle },
      caption: step.pageTitle || step.excerpt || step.url,
    },
  }
}

/** editor -> Entity(file): an identity-bearing artifact with a content peek. */
function editorSurface(step: NeoStep): Surface {
  const fields: EntityField[] = []
  if (step.language) fields.push({ key: 'language', value: step.language })
  if (step.preview) fields.push({ key: 'preview', value: step.preview.slice(0, 280) })
  const affordances: Affordance[] = step.path
    ? [{ id: `${step.id}:path`, label: step.path, kind: 'copy' }]
    : []
  return {
    kind: 'entity',
    id: step.id,
    entity: {
      type: 'file',
      identity: step.path || step.id,
      label: baseName(step.path),
      fields,
      affordances,
    },
  }
}

/** Anything else (a generic tool action) -> a minimal Entity. */
function actionSurface(step: NeoStep): Surface {
  return {
    kind: 'entity',
    id: step.id,
    entity: { type: step.kind, identity: step.id, label: step.title || 'Action' },
  }
}

function stepToSurface(step: NeoStep): Surface | null {
  switch (step.kind) {
    case 'narration':
      if (!step.text) return null
      return { kind: 'narration', id: step.id, narration: { text: step.text, role: 'thinking' } }
    case 'terminal':
      return terminalSurface(step)
    case 'browser':
      return browserSurface(step)
    case 'editor':
      return editorSurface(step)
    // search/media steps are mere running indicators — the rich payload rides
    // on task.searches / task.media, projected by the aggregate mappers below.
    case 'search':
    case 'media':
      return null
    default:
      return actionSurface(step)
  }
}

/** search -> a Narration(answer) + a Structure(list) of source records. */
function searchSurfaces(search: NeoSearch, i: number): Surface[] {
  const out: Surface[] = []
  if (search.answer) {
    out.push({ kind: 'narration', id: `search:${i}:answer`, narration: { text: search.answer } })
  }
  if (search.results.length > 0) {
    const records: StructureNode[] = search.results.map((r, ri) => ({
      id: `search:${i}:src:${ri}`,
      label: r.title || r.url,
      ref: r.url,
      cells: { snippet: r.snippet, url: r.url, published: r.published || '' },
    }))
    out.push({
      kind: 'structure',
      id: `search:${i}:sources`,
      structure: { shape: 'list', records },
    })
  }
  return out
}

/** media -> Canvas(image|video). */
function mediaSurface(item: ChatMedia, i: number): Surface {
  const kind: MediaKind = item.kind === 'video' ? 'video' : 'image'
  return {
    kind: 'canvas',
    id: `media:${i}`,
    canvas: {
      media: { kind, url: item.url, mime: item.mime, alt: item.prompt },
      caption: item.prompt,
    },
  }
}

function swarmStatus(s: NeoSwarm['agents'][number]['status']): StepStatus {
  return s === 'done' ? 'done' : s === 'failed' ? 'failed' : 'running'
}

/** swarm -> a single Timeline, one stateful step per sub-agent. */
function swarmSurface(swarm: NeoSwarm): Surface {
  const steps: TimelineStep[] = swarm.agents.map((a) => ({
    id: `agent:${a.index}`,
    label: a.name,
    status: swarmStatus(a.status),
    detail: a.summary || a.activity || a.task,
  }))
  return { kind: 'timeline', id: 'swarm', timeline: { title: 'Agent Swarm', steps } }
}

/**
 * neoTaskToSurfaces projects a legacy NeoTask onto an ordered list of Construct
 * surfaces. Order mirrors the run's chronology: workspace steps, then search
 * results, generated media, the swarm, and finally the settled answer.
 */
export function neoTaskToSurfaces(task: NeoTask): Surface[] {
  const surfaces: Surface[] = []
  for (const step of task.steps) {
    const s = stepToSurface(step)
    if (s) surfaces.push(s)
  }
  task.searches.forEach((search, i) => surfaces.push(...searchSurfaces(search, i)))
  task.media.forEach((item, i) => surfaces.push(mediaSurface(item, i)))
  if (task.swarm) surfaces.push(swarmSurface(task.swarm))
  if (task.done && task.answer) {
    surfaces.push({
      kind: 'narration',
      id: 'answer',
      narration: { text: task.answer, role: 'answer' },
    })
  }
  return surfaces
}
