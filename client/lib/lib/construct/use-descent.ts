'use client'

/**
 * useDescent — the React wiring that drives depth navigation on the shared
 * `SurfaceWorkspace` (design "Depth navigation", R4.x, R17.3/R17.4).
 *
 * The shell owns the workspace state; this hook turns a tap on a Timeline step
 * into a focus-stack mutation, applying the PURE operations from
 * `lib/construct/focus.ts` through the owner's `setWorkspace`. It keeps all the
 * stack logic in the pure module and only adds the React glue: an in-flight
 * guard (so a second tap can't race a pending cold rehydration), the non-jargon
 * failure notice (R4.5/R12), and the ascend handler.
 *
 * Cold-link rehydration (R4.4): a tapped step whose linked `Stream` is not in
 * the hot set is rehydrated BY ADDRESS first, bounded to 5s (R4.5), via the
 * injected `rehydrate` dependency the shell owner supplies (the feed-backed
 * rehydration is wired in task 8.1; until then a cold link reports unavailable
 * in plain language rather than hanging).
 */
import { useCallback, useState } from 'react'
import {
  ascend as ascendWorkspace,
  descendToLinkedStream,
  type DescentDeps,
  type Linkable,
} from '@/lib/construct/focus'
import type { SurfaceWorkspace } from '@/lib/construct/workspace'

export interface UseDescentResult {
  /** Descend from a tapped Timeline step into its linked `Stream` at raw (R17.4). */
  descend: (step: Linkable) => void
  /** Pop the top focus frame (ascend); a no-op at the base (R4.7/R4.8). */
  ascend: () => void
  /** A non-jargon notice when a descent could not open the detail (R4.5/R12). */
  notice: string | null
  /** Clear the descent notice. */
  clearNotice: () => void
  /** True while a cold-link rehydration is in flight (guards re-entrancy). */
  pending: boolean
}

/**
 * useDescent wires the focus stack to the owner's workspace state.
 *
 * @param workspace    the current shared model (read for the latest stack).
 * @param setWorkspace commit a new workspace (the focus mutation lands here).
 * @param deps         cold-link rehydration + budget (R4.4/R4.5); optional.
 */
export function useDescent(
  workspace: SurfaceWorkspace,
  setWorkspace: (ws: SurfaceWorkspace) => void,
  deps: DescentDeps = {},
): UseDescentResult {
  const [notice, setNotice] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const descend = useCallback(
    (step: Linkable) => {
      // Re-entrancy guard: ignore a new tap while a cold rehydration is racing.
      if (pending) return
      setPending(true)
      // Resolve against the workspace at tap time; descendToLinkedStream never
      // mutates it and returns the input UNCHANGED on a no-op/failure (R4.5/R4.6).
      void descendToLinkedStream(workspace, step, deps)
        .then((result) => {
          if (result.status === 'descended') {
            setNotice(null)
            setWorkspace(result.workspace)
          } else if (result.status === 'unavailable') {
            // Focus stack left unchanged; surface the plain-language reason.
            setNotice(result.message ?? null)
          }
          // 'noop': nothing to open — leave the stack and notice as they are.
        })
        .finally(() => setPending(false))
    },
    [workspace, setWorkspace, deps, pending],
  )

  const ascend = useCallback(() => {
    setWorkspace(ascendWorkspace(workspace))
  }, [workspace, setWorkspace])

  const clearNotice = useCallback(() => setNotice(null), [])

  return { descend, ascend, notice, clearNotice, pending }
}
