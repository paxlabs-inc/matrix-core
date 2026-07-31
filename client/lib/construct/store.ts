/**
 * Construct transport consumer — the client mirror of
 * `construct/internal/transport` (Go).
 *
 * The daemon streams two event types over the existing SSE pipe:
 *
 *   construct.surface        a full surface (one of the 8 frozen primitives)
 *   construct.surface.patch  a progressive update to an existing surface id
 *
 * This module turns those raw SSE rows into an ordered, deduplicated list of
 * `Surface`s the renderers consume. `applyPatch` is a byte-for-byte mirror of
 * the canonical Go `ApplyPatch` (Stream = append+dedup-by-chunk-seq, Timeline
 * = upsert-by-step-id, every other kind = full payload replace) so the live
 * tap and a replay converge to exactly the same state.
 *
 * It NEVER trusts the wire blindly: `parseSurface` validates the structural
 * contract (known kind, non-empty id, exactly one payload matching the kind)
 * before a surface is admitted — the client half of invariant i2 ("the agent
 * fills trusted primitives, never emits arbitrary UI").
 */
import type {
  Surface,
  Stream as StreamPayload,
  Timeline as TimelinePayload,
  Kind,
} from '@/lib/construct/types.gen'

export const CONSTRUCT_SURFACE = 'construct.surface'
export const CONSTRUCT_SURFACE_PATCH = 'construct.surface.patch'

const KINDS: ReadonlySet<Kind> = new Set<Kind>([
  'narration',
  'metric',
  'entity',
  'structure',
  'stream',
  'timeline',
  'canvas',
  'ask',
])

/** The single payload field selected by each kind (mirrors the Go envelope). */
const PAYLOAD_KEY: Record<Kind, keyof Surface> = {
  narration: 'narration',
  metric: 'metric',
  entity: 'entity',
  structure: 'structure',
  stream: 'stream',
  timeline: 'timeline',
  canvas: 'canvas',
  ask: 'ask',
}

function isKind(v: unknown): v is Kind {
  return typeof v === 'string' && KINDS.has(v as Kind)
}

/**
 * validateSurface checks the structural contract a trusted renderer relies on:
 * a known kind, a non-empty id, and exactly one payload matching the kind.
 * Mirrors `schema.Surface.Validate` (the envelope-level checks). Deep payload
 * semantics are the renderers' concern; this is the cheap transport gate.
 */
export function validateSurface(s: unknown): s is Surface {
  if (!s || typeof s !== 'object') return false
  const obj = s as Record<string, unknown>
  if (!isKind(obj.kind)) return false
  if (typeof obj.id !== 'string' || obj.id === '') return false
  let payloads = 0
  for (const kind of KINDS) {
    if (obj[PAYLOAD_KEY[kind]] != null) payloads++
  }
  if (payloads !== 1) return false
  return obj[PAYLOAD_KEY[obj.kind]] != null
}

/**
 * parseSurface extracts and validates a surface from an SSE event's `fields`
 * payload (the `surface` key carries the full or increment surface). Returns
 * null when the field is missing or fails the structural contract — a hostile
 * or malformed payload is dropped, never rendered.
 */
export function parseSurface(fields: Record<string, unknown> | undefined): Surface | null {
  if (!fields) return null
  const raw = fields.surface
  if (!raw) return null
  if (!validateSurface(raw)) return null
  return raw
}

/** Deep-clone without normalizing numbers or dropping explicit keys. */
function clone<T>(v: T): T {
  return structuredClone(v)
}

/**
 * applyPatch folds a `construct.surface.patch` increment onto an existing
 * surface, returning a NEW surface (the base is never mutated). Exact mirror
 * of the canonical Go `ApplyPatch`:
 *
 *   - Stream:   APPEND patch chunks (dedup by chunk seq -> idempotent on
 *     replay); `closed` latches true; non-empty source/title overwrite.
 *   - Timeline: UPSERT patch steps by step id; non-empty title overwrites.
 *   - all other kinds: REPLACE the payload wholesale (full re-render).
 *
 * Envelope decoration on the patch (ref/seq/parent/attributes) is applied when
 * present. The patch must match the base kind; on a mismatch the base is
 * returned unchanged (the renderer keeps the last good state).
 */
export function applyPatch(base: Surface, patch: Surface): Surface {
  if (base.kind !== patch.kind) return base
  const out = clone(base)

  if (patch.ref) out.ref = patch.ref
  if (patch.seq) out.seq = patch.seq
  if (patch.parent) out.parent = patch.parent
  if (patch.attributes) out.attributes = patch.attributes

  switch (base.kind) {
    case 'stream':
      mergeStream(out.stream, patch.stream)
      break
    case 'timeline':
      mergeTimeline(out.timeline, patch.timeline)
      break
    default:
      out.narration = patch.narration
      out.metric = patch.metric
      out.entity = patch.entity
      out.structure = patch.structure
      out.canvas = patch.canvas
      out.ask = patch.ask
      break
  }
  return out
}

function mergeStream(base: StreamPayload | undefined, patch: StreamPayload | undefined) {
  if (!base || !patch) return
  if (patch.source) base.source = patch.source
  if (patch.title) base.title = patch.title
  const seen = new Set<number>()
  base.chunks = base.chunks ?? []
  for (const c of base.chunks) seen.add(c.seq)
  for (const c of patch.chunks ?? []) {
    if (seen.has(c.seq)) continue
    seen.add(c.seq)
    base.chunks.push(c)
  }
  if (patch.closed) base.closed = true
}

function mergeTimeline(base: TimelinePayload | undefined, patch: TimelinePayload | undefined) {
  if (!base || !patch) return
  if (patch.title) base.title = patch.title
  base.steps = base.steps ?? []
  const idx = new Map<string, number>()
  base.steps.forEach((s, i) => idx.set(s.id, i))
  for (const s of patch.steps ?? []) {
    const i = idx.get(s.id)
    if (i !== undefined) {
      base.steps[i] = s
      continue
    }
    idx.set(s.id, base.steps.length)
    base.steps.push(s)
  }
}

/**
 * applySurfaceEvent folds a single construct.surface[.patch] event into an
 * ordered surface list, returning a NEW list (immutable update for React).
 *
 *   - construct.surface       : upsert by id (full replace if seen, else append)
 *   - construct.surface.patch : merge onto the existing base via applyPatch;
 *     if the base hasn't arrived yet, the increment seeds it (lenient — a
 *     reconnect can deliver a patch before its full surface).
 *
 * Insertion order is preserved; an upsert/patch updates in place so a surface
 * never jumps position as it streams.
 */
export function applySurfaceEvent(list: Surface[], type: string, surface: Surface): Surface[] {
  const i = list.findIndex((s) => s.id === surface.id)
  if (type === CONSTRUCT_SURFACE_PATCH && i >= 0) {
    const next = list.slice()
    next[i] = applyPatch(list[i], surface)
    return next
  }
  if (i >= 0) {
    const next = list.slice()
    next[i] = surface
    return next
  }
  return [...list, surface]
}
