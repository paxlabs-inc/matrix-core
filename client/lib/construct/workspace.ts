/**
 * Surface-State Model (the SHARED CORE) — `SurfaceWorkspace` + the deterministic
 * `placeSurface` placement policy.
 *
 * This is the single client-side shell-state model both shell adapters read
 * (Desktop Web windowed/spatial; Mobile PWA app-grid). It is *additive over* the
 * frozen `Surface` envelope (`types.gen.ts`): it NEVER changes a `Surface`; it
 * wraps each one with shell-level placement/lifecycle metadata and indexes them
 * for addressing, focus (depth), pins, the Ask inbox, and the Metric tray.
 *
 * `placeSurface` is a PURE, deterministic function: equal `(surface, prior)`
 * inputs always yield an equal `Placement`. `region` is a pure function of the
 * surface `kind` over the 7 OS zones; a prior `pinned`/`lifecycle`/layout slot is
 * preserved so user pins survive re-projection; an unknown kind lands in a
 * deterministic default region rather than throwing or being left unplaced.
 *
 * Like `store.ts`, this module is mirrored faithfully into `apps/mobile`
 * (`src/lib/matrix/construct/workspace.ts`, import path adjusted) so both the web
 * and mobile clients read an identical model. The Go `construct/schema` remains
 * the source of truth for the `Surface` envelope; only `types.gen.ts` is codegen'd.
 */
import type { Surface, Kind } from '@/lib/construct/types.gen'
import { applyPatch, CONSTRUCT_SURFACE_PATCH } from '@/lib/construct/store'

/**
 * The 7 OS zones a surface can occupy (design "Surface → OS concept mapping"):
 *
 *   home       Structure/Entity grid — the finder / app-grid / home arrangement
 *   window     Canvas focus — a focusable window (web page / image / doc)
 *   drawer     Stream — a power drawer (terminal / logs), collapsed by default
 *   activity   Timeline — glanceable awareness of what Neo is doing
 *   tray       Metric — cost / spend / progress in the persistent status area
 *   inbox      Ask — environment-level, async trust back-channel
 *   narration  Narration — "Neo talking"; one panel (chat), never the page
 */
export type Region = 'home' | 'window' | 'drawer' | 'activity' | 'tray' | 'inbox' | 'narration'

/** Every valid placement region, in canonical order (for validation/iteration). */
export const REGIONS: readonly Region[] = [
  'home',
  'window',
  'drawer',
  'activity',
  'tray',
  'inbox',
  'narration',
] as const

/**
 * A surface lifecycle stage:
 *   live     currently patching (the run is actively emitting onto it)
 *   settled  the run is done but the surface is retained in the hot set
 *   archived aged out of the hot set but still re-enterable from history
 */
export type Lifecycle = 'live' | 'settled' | 'archived'

/** The three shell-level depth levels, in ascending order. */
export type FocusLevel = 'glance' | 'summary' | 'raw'

/**
 * The deterministic default region for a surface whose `kind` is not one of the
 * 8 frozen primitives. The shell never throws on an unknown kind and never leaves
 * a surface unplaced (R2.7) — it parks it in `home`, the default arrangement.
 */
export const DEFAULT_REGION: Region = 'home'

/**
 * Placement is shell metadata wrapping a surface: which OS zone it lives in and
 * its lifecycle, plus the two adapter-interpreted layout slots. It is derived by
 * `placeSurface` and persisted alongside the surface so the arrangement is durable.
 */
export interface Placement {
  /** Which OS zone the surface occupies. Pure function of the surface `kind`. */
  region: Region
  /** Pinned surfaces ("apps you return to") persist across runs and survive cleanup. */
  pinned: boolean
  /** live | settled | archived. */
  lifecycle: Lifecycle
  /** Desktop window stacking order. The only field the desktop adapter interprets. */
  zOrder?: number
  /** Mobile home-grid slot. The only field the mobile adapter interprets. */
  gridSlot?: number
}

/**
 * A `Surface` envelope wrapped with shell placement/lifecycle and an address.
 * The `surface` field is the frozen envelope, UNCHANGED.
 */
export interface PlacedSurface {
  /** The frozen `Surface` envelope, never mutated by the shell. */
  surface: Surface
  /** Where this surface lives in the environment. */
  placement: Placement
  /** Stable re-enterable address: `construct://{conversationId}/{surfaceId}`. */
  address: string
  /** The highest frame `seq` that has updated this surface (for dedup/ordering). */
  updatedSeq: number
}

/** One frame on the depth-navigation focus stack. */
export interface FocusFrame {
  surfaceId: string
  level: FocusLevel
}

/** The shell-level depth stack (glance → summary → raw). */
export interface FocusState {
  stack: FocusFrame[]
}

/**
 * A `construct.surface.patch` increment whose base surface had not yet arrived
 * when it was received. It is HELD (never dropped) until its base surface lands,
 * tagged with the publishing frame `seq` so buffered patches fold in `seq` order
 * and a repeated frame dedups by `seq` (R1.6/R1.7).
 */
export interface BufferedPatch {
  /** The publishing frame `seq` (dedup + ordering key). */
  seq: number
  /** The patch surface envelope (a deep, caller-independent copy). */
  surface: Surface
}

/** An entry in the environment-level Ask inbox. */
export interface AskInboxEntry {
  surfaceId: string
  pending: boolean
  /** ISO-8601 expiry carried by the Ask surface, when present. */
  expiresAt?: string
}

/**
 * The single shared source of client-side shell state both adapters read. Wraps
 * each `Surface` with placement/lifecycle and indexes for addressing, focus, pins,
 * the Ask inbox, and the Metric tray.
 */
export interface SurfaceWorkspace {
  conversationId: string
  /** All placed surfaces, keyed by surface id. */
  surfaces: Map<string, PlacedSurface>
  /** The depth-navigation focus stack. */
  focus: FocusState
  /** Ids of pinned surfaces. */
  pins: string[]
  /** Pending/expired Ask inbox entries. */
  asks: AskInboxEntry[]
  /** Metric surface ids shown in the status tray. */
  tray: string[]
  /**
   * Orphan-patch buffer: `construct.surface.patch` increments whose base surface
   * has not yet arrived, keyed by target surface id and held in `seq` order. A
   * patch is NEVER dropped for arriving early (reconnect/out-of-order tolerance);
   * it folds in when the base lands and is otherwise retained for a later frame
   * (R1.6/R1.7). Additive shell bookkeeping — not part of the frozen envelope.
   */
  pending: Map<string, BufferedPatch[]>
  /** Highest applied frame `seq`; monotonically non-decreasing. */
  lastSeq: number
}

/** The 8 frozen primitive kinds → their home OS zone. */
const REGION_BY_KIND: Record<Kind, Region> = {
  narration: 'narration',
  metric: 'tray',
  entity: 'home',
  structure: 'home',
  stream: 'drawer',
  timeline: 'activity',
  canvas: 'window',
  ask: 'inbox',
}

/**
 * regionForSurface derives the OS zone for a surface purely from its `kind`
 * (a function of `kind` is, trivially, also a function of `(kind, attributes)` —
 * the policy reserves attribute-sensitivity for inbox routing).
 *
 *   - `narration` ALWAYS → `narration`, regardless of attributes or prior (R3.3).
 *   - `ask` → `inbox`; an Ask carrying `stakes ∈ {decision, irreversible}` is
 *     thereby guaranteed to land in the inbox (design `placeSurface` postcondition).
 *   - an unknown/forged kind → the deterministic `DEFAULT_REGION` (`home`),
 *     never throwing and never leaving the surface unplaced (R2.7).
 */
function regionForSurface(surface: Surface): Region {
  // Guard against inherited Object.prototype keys ('__proto__', 'constructor',
  // 'toString', …): a forged/unknown `kind` that collides with one would otherwise
  // resolve to a truthy non-Region value (a function/object) and slip past the
  // `?? DEFAULT_REGION` fallback. An own-key check guarantees an unknown kind lands
  // in the deterministic DEFAULT_REGION, never a non-region value (R2.7).
  if (Object.prototype.hasOwnProperty.call(REGION_BY_KIND, surface.kind)) {
    return REGION_BY_KIND[surface.kind]
  }
  return DEFAULT_REGION
}

/**
 * placeSurface derives a `Placement` for a surface deterministically.
 *
 * Guarantees (design "Key Functions → placeSurface", R2.4–2.7, R3.3):
 *   - Deterministic: equal `(surface, prior)` always yields a field-for-field
 *     equal `Placement`.
 *   - `region` is a pure function of `surface.kind` over the 7 regions; it is
 *     recomputed every time and is NEVER taken from `prior` (so `narration`
 *     re-projects to `narration` even if a stale prior said otherwise).
 *   - A `prior.pinned` is preserved (user pins survive re-projection).
 *   - `prior.lifecycle` and the layout slots (`zOrder`/`gridSlot`) are preserved
 *     when a prior is supplied; a freshly placed surface defaults to `live`.
 *   - An unknown kind is placed in the default region — never throws.
 */
export function placeSurface(surface: Surface, prior?: Placement): Placement {
  return {
    region: regionForSurface(surface),
    pinned: prior?.pinned ?? false,
    lifecycle: prior?.lifecycle ?? 'live',
    zOrder: prior?.zOrder,
    gridSlot: prior?.gridSlot,
  }
}

/**
 * surfaceAddress builds the stable, re-enterable address for a surface:
 * `construct://{conversationId}/{surfaceId}`. The address is derived from the
 * persisted identity (not the live connection) so it is identical across reload,
 * rehydration, and device switch (R7.1).
 */
export function surfaceAddress(conversationId: string, surfaceId: string): string {
  return `construct://${conversationId}/${surfaceId}`
}

/**
 * groupByRegion buckets a workspace's placed surfaces by their OS zone, so a
 * shell adapter can lay each region out independently. The result has an entry
 * for EVERY region (empty arrays included), so an adapter never has to guard for
 * a missing key, and surfaces keep their workspace insertion order within a
 * region. Pure read over the shared model — both adapters use it.
 */
export function groupByRegion(ws: SurfaceWorkspace): Record<Region, PlacedSurface[]> {
  const out: Record<Region, PlacedSurface[]> = {
    home: [],
    window: [],
    drawer: [],
    activity: [],
    tray: [],
    inbox: [],
    narration: [],
  }
  for (const placed of ws.surfaces.values()) {
    out[placed.placement.region].push(placed)
  }
  return out
}

/**
 * pushFocus appends a single `FocusFrame` to the depth stack, returning a NEW
 * workspace (immutable update). The surfaces/pins/tray/etc. are carried by
 * reference; only the focus stack is replaced. This is the one-frame-deeper
 * primitive depth navigation builds on (see `lib/construct/focus.ts`).
 */
export function pushFocus(ws: SurfaceWorkspace, frame: FocusFrame): SurfaceWorkspace {
  return { ...ws, focus: { stack: [...ws.focus.stack, frame] } }
}

/**
 * popFocus removes the top `FocusFrame` from the depth stack, returning a NEW
 * workspace. At the base (empty stack) it returns the workspace UNCHANGED (same
 * reference), so an ascend at the base is a true no-op (R4.8).
 */
export function popFocus(ws: SurfaceWorkspace): SurfaceWorkspace {
  if (ws.focus.stack.length === 0) return ws
  return { ...ws, focus: { stack: ws.focus.stack.slice(0, -1) } }
}

/** Construct an empty workspace for a conversation (no surfaces, empty indexes). */
export function emptyWorkspace(conversationId: string): SurfaceWorkspace {
  return {
    conversationId,
    surfaces: new Map<string, PlacedSurface>(),
    focus: { stack: [] },
    pins: [],
    asks: [],
    tray: [],
    pending: new Map<string, BufferedPatch[]>(),
    lastSeq: 0,
  }
}

/** Deep-clone a surface without normalizing numbers or dropping explicit keys. */
function cloneSurface(s: Surface): Surface {
  return structuredClone(s)
}

/** lastSeq is monotonically non-decreasing: never moves backward (R1.5). */
function bumpSeq(prev: number, seq: number): number {
  return seq > prev ? seq : prev
}

/**
 * setPlaced writes a single `PlacedSurface` and advances `lastSeq`, touching ONLY
 * the targeted surface (a fresh `surfaces` Map with one entry replaced). The rest
 * of the workspace — every other surface, focus, pins, tray — is carried by
 * reference, so a patch never triggers a full re-layout (R16.4).
 */
function setPlaced(
  ws: SurfaceWorkspace,
  id: string,
  placed: PlacedSurface,
  seq: number,
): SurfaceWorkspace {
  const surfaces = new Map(ws.surfaces)
  surfaces.set(id, placed)
  return { ...ws, surfaces, lastSeq: bumpSeq(ws.lastSeq, seq) }
}

/**
 * bufferPatch HOLDS an orphan `construct.surface.patch` whose base surface is not
 * yet present, keyed by target id (R1.6). Buffered patches dedup by frame `seq`
 * so re-receiving the same frame is a no-op (idempotence, R1.4). The held patch is
 * a deep copy, independent of the caller's envelope (clone-on-write, R6.1).
 */
function bufferPatch(ws: SurfaceWorkspace, surface: Surface, seq: number): SurfaceWorkspace {
  const held = ws.pending.get(surface.id) ?? []
  if (held.some((bp) => bp.seq === seq)) {
    // Already buffered this exact frame: `lastSeq` already covers it, so the
    // workspace is unchanged — return it as-is for true idempotence.
    return ws
  }
  const pending = new Map(ws.pending)
  pending.set(surface.id, [...held, { seq, surface: cloneSurface(surface) }])
  return { ...ws, pending, lastSeq: bumpSeq(ws.lastSeq, seq) }
}

/**
 * drainPending folds any buffered orphan patches for a just-arrived base surface,
 * in ascending `seq` order, via the same `applyPatch` the live feed uses. A
 * buffered patch older than the base it would fold onto is stale (its base was
 * superseded) and is discarded; all others are applied and removed from the
 * buffer. A patch with no base to fold onto is never reached here, so it stays
 * retained for a later frame (R1.7).
 */
function drainPending(ws: SurfaceWorkspace, id: string): SurfaceWorkspace {
  const held = ws.pending.get(id)
  if (!held || held.length === 0) return ws
  const base = ws.surfaces.get(id)
  if (!base) return ws

  let cur = base
  const ordered = [...held].sort((a, b) => a.seq - b.seq)
  for (const bp of ordered) {
    if (bp.seq <= cur.updatedSeq) continue // stale relative to the live base
    const merged = applyPatch(cur.surface, bp.surface)
    cur = {
      surface: merged,
      placement: placeSurface(merged, cur.placement),
      address: cur.address,
      updatedSeq: bp.seq,
    }
  }

  const surfaces = new Map(ws.surfaces)
  surfaces.set(id, cur)
  const pending = new Map(ws.pending)
  pending.delete(id) // every held patch was either folded or superseded by the base
  return { ...ws, surfaces, pending, lastSeq: bumpSeq(ws.lastSeq, cur.updatedSeq) }
}

/**
 * applySurfaceEvent folds a single construct.surface[.patch] frame into the shared
 * `SurfaceWorkspace`, returning a NEW workspace (immutable update). It is the
 * workspace-level reducer the shell reads, reusing the byte-for-byte `applyPatch`
 * semantics of `store.ts` (Stream append+dedup-by-chunk-seq, Timeline
 * upsert-by-step-id, every other kind full payload replace) and layering on the
 * shell-state behaviours the environment needs:
 *
 *   - clone-on-write: the stored `Surface` envelope is a deep copy and the input
 *     envelope is NEVER mutated, so a stored envelope stays deep-equal to its
 *     validated input (R6.1). `applyPatch` likewise clones its base.
 *   - orphan-patch buffering: a patch whose base id is absent is BUFFERED (in
 *     `seq` order, deduped by `seq`), then folded when the base arrives — never
 *     dropped, never misapplied (R1.6/R1.7).
 *   - idempotence on replay: re-applying a frame at or below a surface's applied
 *     `seq` (or a frame already buffered) is a no-op, so applying the same frame
 *     twice equals applying it once (R1.4).
 *   - monotonic `lastSeq`: set to the highest applied frame `seq`, never moving
 *     backward (R1.5).
 *   - placement: each surface is wrapped into a `PlacedSurface` via `placeSurface`,
 *     preserving the prior placement (so user pins survive) and carrying the stable
 *     `construct://` address.
 *   - in-place update: a patch touches ONLY its targeted surface, never re-laying
 *     out the rest of the environment (R16.4).
 *
 * `type` is the SSE event type (`construct.surface` | `construct.surface.patch`),
 * `surface` the parsed/validated envelope, and `seq` the publishing frame `seq`.
 */
export function applySurfaceEvent(
  ws: SurfaceWorkspace,
  type: string,
  surface: Surface,
  seq: number,
): SurfaceWorkspace {
  const existing = ws.surfaces.get(surface.id)

  if (type === CONSTRUCT_SURFACE_PATCH) {
    // Orphan patch: base not present yet — hold it rather than seed/drop it.
    if (!existing) return bufferPatch(ws, surface, seq)
    // Replay/stale guard: a frame already reflected in this surface is a no-op.
    if (seq <= existing.updatedSeq) return ws
    const merged = applyPatch(existing.surface, surface)
    const placed: PlacedSurface = {
      surface: merged,
      placement: placeSurface(merged, existing.placement),
      address: existing.address,
      updatedSeq: seq,
    }
    return setPlaced(ws, surface.id, placed, seq)
  }

  // Full surface (construct.surface): upsert with clone-on-write, preserving a
  // prior placement so pins/lifecycle/layout-slots survive re-projection.
  if (existing && seq <= existing.updatedSeq) return ws
  const stored = cloneSurface(surface)
  const placed: PlacedSurface = {
    surface: stored,
    placement: placeSurface(stored, existing?.placement),
    address: existing?.address ?? surfaceAddress(ws.conversationId, surface.id),
    updatedSeq: seq,
  }
  // Place the base, then fold any orphan patches that were waiting on it.
  return drainPending(setPlaced(ws, surface.id, placed, seq), surface.id)
}
