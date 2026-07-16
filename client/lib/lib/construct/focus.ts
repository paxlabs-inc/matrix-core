/**
 * Depth navigation — the shell-level FOCUS STACK and the operations that drive
 * it (design "Depth navigation", R4.x, R17.3/R17.4).
 *
 * Depth is a SHELL concern, not a per-renderer one: the environment keeps a
 * stack of `FocusFrame`s on the shared `SurfaceWorkspace`, and the SAME reused
 * per-primitive renderer draws whichever surface the top frame targets at the
 * frame's level. The base of the stack is the implicit `glance` of every placed
 * surface; pushing a frame descends one level deeper (glance → summary → raw),
 * popping ascends back.
 *
 * This module keeps the focus-stack logic as PURE functions on the model wherever
 * possible (`resolveLink`, `pushRawFocus`, `ascend`, `topFocus`), so the unit
 * test (task 6.4) can drive them deterministically. The ONE operation that must
 * touch the outside world is the cold-link descent: when a tapped Timeline step
 * links to a `Stream` that is not in the hot set, the linked surface is
 * rehydrated BY ADDRESS first (R4.4), bounded to 5 seconds (R4.5). That single
 * async path (`descendToLinkedStream`) takes its rehydration as an injected
 * dependency, so it too is unit-testable without a live transport.
 *
 * MVP concrete path (task 6.3 / R17.4): tap a Timeline STEP → resolve that
 * step's link by `ref` first, else `parent`, to its target `Stream` surface →
 * push a RAW focus frame targeting the linked Stream. A step with no link is a
 * no-op (R4.6); ascending pops exactly the top frame and is a no-op at the base
 * (R4.7/R4.8).
 */
import type { FocusFrame, SurfaceWorkspace } from '@/lib/construct/workspace'
import { pushFocus, popFocus } from '@/lib/construct/workspace'

/**
 * The rehydration budget for a cold descend target: if the linked surface does
 * not become available within this window, the descent fails and the focus
 * stack is left UNCHANGED (R4.5).
 */
export const DESCENT_TIMEOUT_MS = 5000

/**
 * The consumer-readable, NON-JARGON message shown when a descent cannot open the
 * linked detail (cold link that failed to rehydrate, or timed out). It states
 * the outcome ("couldn't open that detail") with no protocol jargon and no error
 * code or stack trace (R12.1/R12.3, R4.5).
 */
export const DESCENT_UNAVAILABLE_MESSAGE =
  "Couldn't open that detail right now. Try again in a moment."

/**
 * Anything carrying the envelope's two link pointers. A `Surface` and a
 * `TimelineStep` both satisfy this shape (each carries an optional `ref`; a
 * surface also carries `parent`), so `resolveLink` works uniformly over both.
 */
export interface Linkable {
  /** Primary link to another surface (depth/descend follows this first). */
  ref?: string
  /** Composition link (descend falls back to this when `ref` is absent). */
  parent?: string
}

/**
 * resolveLink resolves a link to its target surface id by `ref` FIRST, else by
 * `parent` (R4.3). Returns `undefined` when neither is present (nothing to
 * descend into — the caller treats that as a no-op, R4.6). Pure.
 */
export function resolveLink(link: Linkable | undefined): string | undefined {
  if (!link) return undefined
  return link.ref ?? link.parent ?? undefined
}

/** topFocus returns the deepest (top) focus frame, or `undefined` at the base. */
export function topFocus(ws: SurfaceWorkspace): FocusFrame | undefined {
  return ws.focus.stack[ws.focus.stack.length - 1]
}

/**
 * pushRawFocus pushes exactly ONE focus frame targeting `surfaceId` at the `raw`
 * level (the MVP's single level of descent: a Timeline step → its linked Stream
 * raw, R17.4). Pure: returns a NEW workspace with the frame appended; the rest
 * of the workspace is carried by reference.
 */
export function pushRawFocus(ws: SurfaceWorkspace, surfaceId: string): SurfaceWorkspace {
  return pushFocus(ws, { surfaceId, level: 'raw' })
}

/**
 * ascend pops exactly the top focus frame (R4.7). At the base (empty stack) it is
 * a no-op and returns the workspace UNCHANGED (R4.8). Pure.
 */
export function ascend(ws: SurfaceWorkspace): SurfaceWorkspace {
  return popFocus(ws)
}

/** The outcome of a descend attempt. */
export type DescentStatus =
  /** A raw focus frame was pushed for the resolved target. */
  | 'descended'
  /** Nothing to descend into (no link); the focus stack is unchanged (R4.6). */
  | 'noop'
  /** The linked target could not be opened; the focus stack is unchanged (R4.5). */
  | 'unavailable'

/** The result of `descendToLinkedStream`. */
export interface DescentResult {
  status: DescentStatus
  /**
   * The workspace after the operation. On `descended` it carries the new raw
   * focus frame (and, for a cold target, the rehydrated surface). On `noop` and
   * `unavailable` it is the INPUT workspace, UNCHANGED — the focus stack is left
   * exactly as it was (R4.5/R4.6).
   */
  workspace: SurfaceWorkspace
  /** A non-jargon explanation, present only when `status === 'unavailable'`. */
  message?: string
}

/** Dependencies for the cold-link descent path. */
export interface DescentDeps {
  /**
   * Rehydrate a cold linked surface BY ADDRESS, resolving to a workspace that
   * includes the target once it has landed (R4.4). It is raced against
   * `timeoutMs`; a rejection OR a timeout is treated as a failure and leaves the
   * focus stack unchanged (R4.5). Omitted when no rehydration path is wired (a
   * cold target is then simply unavailable).
   */
  rehydrate?: (surfaceId: string) => Promise<SurfaceWorkspace>
  /** The rehydration budget in ms. Defaults to `DESCENT_TIMEOUT_MS` (5000, R4.5). */
  timeoutMs?: number
}

/**
 * withTimeout races a promise against a timer, rejecting if the budget elapses
 * first. The timer is always cleared so it never leaks, whichever side wins.
 */
function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('descent-timeout')), ms)
    p.then(
      (value) => {
        clearTimeout(timer)
        resolve(value)
      },
      (err) => {
        clearTimeout(timer)
        reject(err)
      },
    )
  })
}

/**
 * descendToLinkedStream performs the MVP's one level of descent: from a tapped
 * Timeline step (or any `Linkable`), resolve its link to a target `Stream`
 * surface and push a RAW focus frame targeting it (R4.3, R17.3/R17.4).
 *
 * Behaviour (R4.3–R4.6):
 *   - No link (`ref`/`parent` both absent) → `noop`; focus stack unchanged (R4.6).
 *   - Target already in the hot set → push the raw focus frame directly.
 *   - Target COLD (not in the workspace) → rehydrate it by address FIRST, bounded
 *     to `timeoutMs` (default 5s), THEN push the raw focus frame onto the
 *     rehydrated workspace (R4.4).
 *   - Rehydration unavailable, times out, fails, or completes without the target
 *     present → `unavailable`; the focus stack is left UNCHANGED and a non-jargon
 *     message is returned (R4.5).
 *
 * The function never mutates its input; it returns a fresh `DescentResult`.
 */
export async function descendToLinkedStream(
  ws: SurfaceWorkspace,
  link: Linkable,
  deps: DescentDeps = {},
): Promise<DescentResult> {
  const targetId = resolveLink(link)

  // Nothing to descend into: leave the focus stack unchanged (R4.6).
  if (!targetId) return { status: 'noop', workspace: ws }

  // Hot set: the linked surface is already present → push raw focus directly.
  if (ws.surfaces.has(targetId)) {
    return { status: 'descended', workspace: pushRawFocus(ws, targetId) }
  }

  // Cold link with no rehydration path wired → cannot open it (R4.5).
  if (!deps.rehydrate) {
    return { status: 'unavailable', workspace: ws, message: DESCENT_UNAVAILABLE_MESSAGE }
  }

  // Cold link: rehydrate by address FIRST, bounded to the 5s budget (R4.4/R4.5).
  const budget = deps.timeoutMs ?? DESCENT_TIMEOUT_MS
  let rehydrated: SurfaceWorkspace
  try {
    rehydrated = await withTimeout(deps.rehydrate(targetId), budget)
  } catch {
    // Timed out or failed → focus stack stays unchanged; signal in non-jargon.
    return { status: 'unavailable', workspace: ws, message: DESCENT_UNAVAILABLE_MESSAGE }
  }

  // Rehydration completed but the target still isn't present → still a failure;
  // leave the ORIGINAL focus stack unchanged (R4.5).
  if (!rehydrated.surfaces.has(targetId)) {
    return { status: 'unavailable', workspace: ws, message: DESCENT_UNAVAILABLE_MESSAGE }
  }

  // Target landed → push the raw focus frame onto the rehydrated workspace (R4.4).
  return { status: 'descended', workspace: pushRawFocus(rehydrated, targetId) }
}
