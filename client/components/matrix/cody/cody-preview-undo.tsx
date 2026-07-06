'use client'

/**
 * Cody preview pane + undo/checkpoint controls (task 6.5).
 *
 * CodyPreviewPane renders the CodyRun.preview state — the router-proxied
 * sandbox URL (req.7 ac_6): a hero viewport in Prototype, a normal pane
 * elsewhere. codyd does not emit preview.* events yet (backend task 3.3), so
 * absent preview is an honest "No preview yet" empty state — never a fabricated
 * URL. Failed/expired states surface the reason and a single re-preview action.
 *
 * CodyUndo drives the snapshot routes (req.14): one-button undo (restore latest
 * snapshot) in Prototype; named checkpoint create + a restore list in
 * Engineer/Architect. Restore is refused (409) while a worker is alive — that
 * case is caught and surfaced honestly, not swallowed.
 *
 * Layers separate by background tone only — no border strokes. Single accent is
 * the `pax` token.
 */
import { useCallback, useEffect, useState } from 'react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import {
  IconExternal,
  IconRefresh,
  IconAlertCircle,
  IconClock,
} from '@/components/matrix/cody/icons'
import { ApiError } from '@/lib/api/client'
import { routerOrigin } from '@/lib/env'
import { getAccessToken } from '@/lib/auth/session'
import {
  createSnapshot,
  listSnapshots,
  restoreSnapshot,
  type CodyMode,
  type SnapshotInfo,
} from '@/lib/api/cody'
import type { CodyRun } from '@/hooks/api/useCody'

/**
 * Resolve a codyd-emitted preview URL into a browser-loadable one. The URL is
 * served by the router's /preview/{user} proxy on the API origin, which is a
 * DIFFERENT origin than this client — so a site-relative "/preview/…" must be
 * made absolute against the router. The router authenticates by JWT; an iframe
 * cannot send an Authorization header, so the token rides as ?access_token=
 * (the proxy then sets a path-scoped cookie for the app's subresource loads).
 * Returns null until the token resolves.
 */
function useAuthedPreviewUrl(url: string | undefined): string | null {
  const [resolved, setResolved] = useState<string | null>(null)
  useEffect(() => {
    let cancelled = false
    if (!url) {
      setResolved(null)
      return
    }
    void (async () => {
      const token = await getAccessToken()
      if (cancelled) return
      const absolute = /^https?:\/\//i.test(url) ? url : `${routerOrigin()}${url}`
      try {
        const u = new URL(absolute)
        if (token) u.searchParams.set('access_token', token)
        setResolved(u.toString())
      } catch {
        setResolved(absolute)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [url])
  return resolved
}

// ── Preview pane ──────────────────────────────────────────────────────────────

export function CodyPreviewPane({
  run,
  hero,
  onRePreview,
}: {
  run: CodyRun
  hero: boolean
  onRePreview?: () => void
}) {
  const preview = run.preview
  const shell = hero
    ? 'bg-surface-secondary flex min-h-0 flex-1 flex-col gap-3 rounded-lg p-4'
    : 'bg-surface-secondary flex flex-col gap-3 rounded-lg p-4'

  return (
    <section className={shell}>
      <div className="flex shrink-0 items-center gap-2">
        <span className="text-muted-foreground font-mono text-xs tracking-wide uppercase">
          Preview
        </span>
        {preview ? (
          <span className="text-muted-foreground ml-auto font-mono text-[10px] uppercase">
            {preview.state}
          </span>
        ) : null}
      </div>

      {!preview ? (
        <EmptyPreview hero={hero} onRePreview={onRePreview} />
      ) : preview.state === 'pending' ? (
        <div className={hero ? 'm-auto' : 'py-6'}>
          <CodyLoader variant="ring" label="Building preview…" className="m-auto" />
        </div>
      ) : preview.state === 'ready' && preview.url ? (
        <ReadyPreview url={preview.url} hero={hero} />
      ) : preview.state === 'ready' ? (
        // ready but no URL yet — treat as still resolving, honestly
        <EmptyPreview
          hero={hero}
          onRePreview={onRePreview}
          label="Preview is ready but no URL was provided yet."
        />
      ) : (
        <BrokenPreview state={preview.state} reason={preview.reason} onRePreview={onRePreview} />
      )}
    </section>
  )
}

function ReadyPreview({ url, hero }: { url: string; hero: boolean }) {
  // `url` is the clean, human-facing address; `authed` is the absolute,
  // token-carrying URL the browser actually loads (cross-origin + JWT).
  const authed = useAuthedPreviewUrl(url)
  if (hero) {
    return (
      <div className="bg-surface-tertiary flex min-h-0 flex-1 flex-col overflow-hidden rounded-md">
        <div className="flex items-center gap-2 px-3 py-2">
          <span className="text-muted-foreground truncate font-mono text-[11px]">{url}</span>
          {authed ? (
            <a
              href={authed}
              target="_blank"
              rel="noreferrer"
              className="text-pax ml-auto inline-flex items-center gap-1 font-mono text-[11px]"
            >
              <IconExternal className="size-3" />
              <span>Open</span>
            </a>
          ) : null}
        </div>
        {authed ? (
          <iframe
            src={authed}
            title="Cody preview"
            className="min-h-0 w-full flex-1 bg-white"
            sandbox="allow-scripts allow-forms allow-same-origin allow-popups"
          />
        ) : (
          <CodyLoader variant="ring" label="Opening preview…" className="m-auto" />
        )}
      </div>
    )
  }
  return (
    <div className="flex flex-col gap-3">
      <p className="text-muted-foreground truncate font-mono text-xs">{url}</p>
      <div>
        <Button size="sm" asChild disabled={!authed}>
          <a href={authed ?? url} target="_blank" rel="noreferrer">
            <IconExternal className="size-3.5" />
            <span>Open preview</span>
          </a>
        </Button>
      </div>
    </div>
  )
}

function BrokenPreview({
  state,
  reason,
  onRePreview,
}: {
  state: 'failed' | 'expired'
  reason?: string
  onRePreview?: () => void
}) {
  const Icon = state === 'expired' ? IconClock : IconAlertCircle
  const iconClass = state === 'expired' ? 'text-muted-foreground' : 'text-destructive'
  const headline = state === 'expired' ? 'Preview expired' : 'Preview failed'
  return (
    <div className="flex flex-col gap-3 py-2">
      <div className="flex items-center gap-2">
        <Icon className={`size-4 shrink-0 ${iconClass}`} />
        <span className="text-sm font-medium">{headline}</span>
      </div>
      {reason ? <p className="text-muted-foreground text-sm">{reason}</p> : null}
      {onRePreview ? (
        <div>
          <Button size="sm" variant="secondary" onClick={onRePreview}>
            <IconRefresh className="size-3.5" />
            <span>Re-preview</span>
          </Button>
        </div>
      ) : null}
    </div>
  )
}

function EmptyPreview({
  hero,
  onRePreview,
  label,
}: {
  hero: boolean
  onRePreview?: () => void
  label?: string
}) {
  return (
    <div
      className={
        hero
          ? 'bg-surface-tertiary m-auto flex w-full max-w-md flex-col items-center gap-3 rounded-md p-8 text-center'
          : 'flex flex-col gap-3 py-4'
      }
    >
      <p className="text-muted-foreground text-sm">
        {label ?? 'No preview yet. Cody builds a live preview when the plan completes.'}
      </p>
      {onRePreview ? (
        <div>
          <Button size="sm" variant="secondary" onClick={onRePreview}>
            <IconRefresh className="size-3.5" />
            <span>Build preview</span>
          </Button>
        </div>
      ) : null}
    </div>
  )
}

// ── Undo / checkpoints ────────────────────────────────────────────────────────

/** Latest snapshot id created during the run (for one-button undo). */
function latestSnapshotId(run: CodyRun): string | undefined {
  let latest: { seq: number; id: string } | undefined
  for (const ev of run.snapshots) {
    if (ev.action !== 'created') continue
    if (!latest || ev.seq > latest.seq) latest = { seq: ev.seq, id: ev.id }
  }
  return latest?.id
}

/** Turn an unknown thrown value into an honest, human message (409 = live worker). */
function restoreErrorMessage(err: unknown): string {
  if (err instanceof ApiError && err.status === 409) {
    return 'Can’t restore while Cody is still working — stop the run first, then undo.'
  }
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return 'Restore failed.'
}

export function CodyUndo({
  run,
  mode,
  onRestored,
}: {
  run: CodyRun
  mode: CodyMode
  onRestored?: () => void
}) {
  if (mode === 'prototype') return <PrototypeUndo run={run} onRestored={onRestored} />
  return <CheckpointManager run={run} onRestored={onRestored} />
}

function PrototypeUndo({ run, onRestored }: { run: CodyRun; onRestored?: () => void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const latest = latestSnapshotId(run)

  const undo = useCallback(async () => {
    if (!latest) return
    setBusy(true)
    setError(null)
    try {
      await restoreSnapshot({ id: latest, conversation_id: run.convID })
      onRestored?.()
    } catch (err) {
      setError(restoreErrorMessage(err))
    } finally {
      setBusy(false)
    }
  }, [latest, run.convID, onRestored])

  return (
    <section className="bg-surface-secondary flex flex-col gap-2 rounded-lg p-4">
      <div className="flex items-center gap-3">
        <Button size="sm" onClick={undo} disabled={busy || !latest}>
          <IconRefresh className="size-3.5" />
          <span>Undo</span>
        </Button>
        {busy ? <CodyLoader variant="ring" size={16} /> : null}
        <span className="text-muted-foreground text-xs">
          {latest ? 'Roll back to the last checkpoint.' : 'Nothing to undo yet.'}
        </span>
      </div>
      {error ? <p className="text-destructive text-sm">{error}</p> : null}
    </section>
  )
}

function CheckpointManager({ run, onRestored }: { run: CodyRun; onRestored?: () => void }) {
  const [label, setLabel] = useState('')
  const [snapshots, setSnapshots] = useState<SnapshotInfo[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)
  const [restoringId, setRestoringId] = useState<string | null>(null)

  const refresh = useCallback(async (signal?: AbortSignal) => {
    setLoadError(null)
    try {
      const list = await listSnapshots(undefined, signal)
      setSnapshots(list)
    } catch (err) {
      if (err instanceof ApiError && err.status === 499) return // aborted
      setLoadError(err instanceof Error ? err.message : 'Could not load checkpoints.')
      setSnapshots([])
    }
  }, [])

  useEffect(() => {
    const ctrl = new AbortController()
    void refresh(ctrl.signal)
    return () => ctrl.abort()
  }, [refresh])

  const create = useCallback(async () => {
    setCreating(true)
    setActionError(null)
    try {
      await createSnapshot({ label: label.trim() || undefined, conversation_id: run.convID })
      setLabel('')
      await refresh()
    } catch (err) {
      setActionError(err instanceof Error ? err.message : 'Could not create the checkpoint.')
    } finally {
      setCreating(false)
    }
  }, [label, run.convID, refresh])

  const restore = useCallback(
    async (id: string) => {
      setRestoringId(id)
      setActionError(null)
      try {
        await restoreSnapshot({ id, conversation_id: run.convID })
        await refresh()
        onRestored?.()
      } catch (err) {
        setActionError(restoreErrorMessage(err))
      } finally {
        setRestoringId(null)
      }
    },
    [run.convID, refresh, onRestored],
  )

  return (
    <section className="bg-surface-secondary flex flex-col gap-3 rounded-lg p-4">
      <span className="text-muted-foreground font-mono text-xs tracking-wide uppercase">
        Checkpoints
      </span>

      <div className="flex items-center gap-2">
        <Input
          value={label}
          placeholder="Name this checkpoint…"
          onChange={(e) => setLabel(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !creating) void create()
          }}
        />
        <Button size="sm" onClick={create} disabled={creating}>
          <span>Checkpoint</span>
        </Button>
      </div>

      {actionError ? <p className="text-destructive text-sm">{actionError}</p> : null}

      {snapshots === null ? (
        <div className="py-2">
          <CodyLoader variant="ring" size={20} className="m-auto" />
        </div>
      ) : loadError ? (
        <p className="text-destructive text-sm">{loadError}</p>
      ) : snapshots.length === 0 ? (
        <p className="text-muted-foreground text-sm">No checkpoints yet.</p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {snapshots.map((snap) => (
            <li
              key={snap.id}
              className="bg-surface-tertiary flex items-center gap-2 rounded-md px-3 py-2"
            >
              <div className="flex min-w-0 flex-col">
                <span className="truncate text-sm">{snap.label || snap.id}</span>
                {snap.created_at ? (
                  <span className="text-muted-foreground font-mono text-[10px]">
                    {snap.created_at}
                  </span>
                ) : null}
              </div>
              <Button
                size="sm"
                variant="secondary"
                className="ml-auto"
                onClick={() => restore(snap.id)}
                disabled={restoringId !== null}
              >
                {restoringId === snap.id ? (
                  <CodyLoader variant="ring" size={14} />
                ) : (
                  <IconRefresh className="size-3.5" />
                )}
                <span>Restore</span>
              </Button>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
