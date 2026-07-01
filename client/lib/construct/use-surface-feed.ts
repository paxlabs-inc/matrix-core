'use client'

/**
 * useSurfaceFeed — the React wiring that connects the durable + live
 * `SurfaceFeed` to the shared `SurfaceWorkspace` the shell renders (task 8.1,
 * R1.2/R7.1/R7.5/R17.1/R17.4/R17.5).
 *
 * This is the seam that makes a COLD OPEN rehydrate: when a conversation is
 * selected, the hook builds a `SurfaceFeed` for it and calls `hydrate`, which
 * GETs the durable record (`GET /construct/state`), replays it through the SAME
 * `applySurfaceEvent` reducer the live feed uses, then tails the live stream
 * since the newest seq. The resulting workspace is mirrored into React state via
 * the feed's `onChange`, so the home/activity surfaces a previous session left
 * behind reappear on reload and survive it (R1.2/R17.2/R17.5). Each surface is
 * assigned its stable `construct://{conversationId}/{surfaceId}` address by the
 * reducer on add (see `applySurfaceEvent` → `surfaceAddress`); the address flows
 * through untouched here — it is not recomputed or duplicated (R7.1/R7.5).
 *
 * Transport invariant (R14): the live tail rides the EXISTING chat SSE transport
 * (`SurfaceFeed`'s default `chatTransport` → `subscribeEvents`). The hook only
 * binds the run scope: it follows the active run's `intentId` from `useChat`,
 * re-binding the live tail as runs come and go WITHOUT a full re-hydrate (so an
 * open environment is never reset mid-conversation). No new agent→client wire
 * path is introduced.
 *
 * Two writers share the workspace: the feed (surface data, from durable + live
 * frames) and depth navigation (the focus stack, React-only shell state mutated
 * by `useDescent` through `setWorkspace`). The feed never touches focus, so the
 * hook PRESERVES the current focus stack across every feed-driven fold — a live
 * frame arriving while a descent is open never collapses it. `setWorkspace`
 * tracks the latest focus so the next fold carries it forward.
 *
 * The hook also backs depth navigation's cold-link rehydration (R4.4): `descend`
 * into a surface not in the hot set calls `rehydrate(surfaceId)`, which re-reads
 * the durable record and folds any missing surface into the live workspace via
 * the feed, returning it so the descent can resolve and open it by address.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { SurfaceFeed, type FeedStatus } from '@/lib/construct/feed'
import { emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'

export interface UseSurfaceFeedResult {
  /** The shared surface-state model the shell reads (fed live + rehydrated). */
  workspace: SurfaceWorkspace
  /** Commit a new workspace — the seam depth navigation mutates focus through. */
  setWorkspace: (ws: SurfaceWorkspace) => void
  /** A non-jargon connection status (`connecting` → `live`) for an indicator. */
  status: FeedStatus
  /**
   * Rehydrate a cold linked surface BY ADDRESS for depth navigation (R4.4):
   * re-reads the durable record and folds any missing surface into the live
   * workspace, returning the resulting workspace. Bound to `useDescent`'s
   * injected `rehydrate` dependency.
   */
  rehydrate: (surfaceId: string) => Promise<SurfaceWorkspace>
}

/**
 * useSurfaceFeed owns the live `SurfaceFeed` for the active conversation and
 * exposes the shared workspace it drives.
 *
 * @param conversationId the conversation whose environment to hydrate (null/empty
 *   yields an empty environment with no feed — the shell still mounts as root).
 * @param intentId the active run scope to tail live (from `useChat`); re-binds
 *   the live tail without re-hydrating when it changes.
 */
export function useSurfaceFeed(
  conversationId: string | null,
  intentId?: string | null,
): UseSurfaceFeedResult {
  const convId = conversationId ?? ''

  const [workspace, setWorkspaceState] = useState<SurfaceWorkspace>(() => emptyWorkspace(convId))
  const [status, setStatus] = useState<FeedStatus>('idle')

  const feedRef = useRef<SurfaceFeed | null>(null)
  // The latest focus stack, carried forward across every feed-driven fold so a
  // live frame never collapses an open descent. Surface data comes from the
  // feed; focus is React-only shell state owned by depth navigation.
  const focusRef = useRef(workspace.focus)
  // The latest committed workspace, read by `rehydrate` when no feed is active.
  const workspaceRef = useRef(workspace)
  workspaceRef.current = workspace

  // The single commit seam. Depth navigation lands here with a new focus stack;
  // tracking it keeps the next feed fold from dropping it.
  const setWorkspace = useCallback((ws: SurfaceWorkspace) => {
    focusRef.current = ws.focus
    workspaceRef.current = ws
    setWorkspaceState(ws)
  }, [])

  // (Re)build the feed and rehydrate when the conversation changes. A cold open
  // lands here: hydrate replays the durable record then tails live, and every
  // fold mirrors into React state with the current focus stack preserved.
  useEffect(() => {
    focusRef.current = { stack: [] }

    if (!convId) {
      // No conversation yet: tear any prior feed down and show an empty
      // environment. The shell still mounts as the route root (R3.5).
      feedRef.current?.close()
      feedRef.current = null
      const empty = emptyWorkspace('')
      workspaceRef.current = empty
      setWorkspaceState(empty)
      setStatus('idle')
      return
    }

    const feed = new SurfaceFeed(convId, {
      intentId: intentId ?? undefined,
      onChange: (ws) => {
        // Surface data from the feed + the live focus stack from depth nav.
        const merged = { ...ws, focus: focusRef.current }
        workspaceRef.current = merged
        setWorkspaceState(merged)
      },
      onStatus: setStatus,
    })
    feedRef.current = feed
    void feed.hydrate(convId)

    return () => {
      feed.close()
      if (feedRef.current === feed) feedRef.current = null
    }
    // Re-hydrate ONLY on a conversation change. The active-run scope is re-bound
    // by the effect below without re-reading the durable record, so switching
    // runs within a conversation never resets the open environment.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [convId])

  // Follow the active run's scope on the live tail as runs start/stop within the
  // conversation, preserving the hydrated workspace (no re-hydrate).
  useEffect(() => {
    feedRef.current?.setLiveScope(intentId ?? undefined)
  }, [intentId])

  // Cold-link rehydration for depth navigation (R4.4/R7.3): fold any missing
  // surface from the durable record into the live workspace and return it.
  const rehydrate = useCallback(async (_surfaceId: string): Promise<SurfaceWorkspace> => {
    const feed = feedRef.current
    if (!feed) return workspaceRef.current
    const refreshed = await feed.refresh()
    // Carry the open focus stack forward onto the refreshed surface data.
    return { ...refreshed, focus: focusRef.current }
  }, [])

  return { workspace, setWorkspace, status, rehydrate }
}
