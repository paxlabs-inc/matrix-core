'use client'

/**
 * Cody History (cody-smoothness task 4.4) — the user's runs, newest first,
 * scoped to the active project. Server-side and cross-device: the list reads
 * GET /conversations (codyd's durable ledgers); the localStorage recent-runs
 * record remains only a fast-path cache, shown while the fetch is in flight
 * and merged in for anything the server does not know (and as the fallback if
 * the server is unreachable). Opening an entry rebuilds the Workspace from
 * its durable trace.
 */
import { useEffect, useMemo, useState } from 'react'

import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconClock, IconAlertCircle } from '@/components/matrix/cody/icons'
import { getConversations, type CodyConversationSummary } from '@/lib/api/cody'
import type { RecentRun } from '@/lib/cody/recent-runs'

export interface HistoryRow {
  convID: string
  title: string
  status?: string
  mode?: string
  updatedAtMs: number
}

/**
 * mergeHistory folds the server list (source of truth) with the local cache:
 * server rows win; local-only rows (e.g. dispatched from this browser while
 * the server list lagged) are appended. Pure, so it is directly testable.
 */
export function mergeHistory(
  server: CodyConversationSummary[],
  cache: RecentRun[],
  projectID?: string,
): HistoryRow[] {
  const rows: HistoryRow[] = server
    .filter((c) => !projectID || c.project === projectID || (!c.project && projectID === 'default'))
    .map((c) => ({
      convID: c.id,
      title: c.title || c.id,
      status: c.status,
      mode: c.mode,
      updatedAtMs: Date.parse(c.updated_at) || 0,
    }))
  const known = new Set(rows.map((r) => r.convID))
  for (const r of cache) {
    if (known.has(r.convID)) continue
    if (projectID && r.projectID !== projectID) continue
    rows.push({ convID: r.convID, title: r.title, updatedAtMs: r.startedAt })
  }
  return rows.sort((a, b) => b.updatedAtMs - a.updatedAtMs)
}

export function CodyHistory({
  projectID,
  cache,
  onOpen,
}: {
  projectID?: string
  cache: RecentRun[]
  onOpen: (convID: string) => void
}) {
  const [server, setServer] = useState<CodyConversationSummary[] | null>(null)
  const [loading, setLoading] = useState(true)
  const [unreachable, setUnreachable] = useState(false)

  useEffect(() => {
    const ctrl = new AbortController()
    setLoading(true)
    setUnreachable(false)
    getConversations(ctrl.signal)
      .then((list) => setServer(list))
      .catch(() => {
        if (!ctrl.signal.aborted) setUnreachable(true)
      })
      .finally(() => {
        if (!ctrl.signal.aborted) setLoading(false)
      })
    return () => ctrl.abort()
  }, [])

  const rows = useMemo(
    () => mergeHistory(server ?? [], cache, projectID),
    [server, cache, projectID],
  )

  if (loading && rows.length === 0) {
    return <CodyLoader variant="ring" label="Loading history…" className="h-full justify-center" />
  }

  if (rows.length === 0) {
    return (
      <div className="text-muted-foreground m-auto flex h-full flex-col items-center justify-center gap-2 p-8 text-sm">
        <IconClock className="size-6 opacity-60" />
        <span>No runs yet. Start one from the Workspace.</span>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2 p-4">
      {unreachable ? (
        <div className="bg-surface-secondary flex items-center gap-2 rounded-lg px-4 py-2.5">
          <IconAlertCircle className="text-muted-foreground size-4 shrink-0" />
          <span className="text-muted-foreground text-xs">
            History service unreachable — showing runs started in this browser.
          </span>
        </div>
      ) : null}
      {rows.map((r) => (
        <button
          key={r.convID}
          type="button"
          onClick={() => onOpen(r.convID)}
          className="bg-surface-secondary hover:bg-surface-hover flex items-center gap-3 rounded-lg px-4 py-3 text-left transition-colors"
        >
          <StatusDot status={r.status} />
          <span className="truncate text-sm">{r.title}</span>
          {r.mode ? (
            <span className="text-muted-foreground shrink-0 font-mono text-[10px] uppercase">
              {r.mode}
            </span>
          ) : null}
          <span className="text-muted-foreground ml-auto shrink-0 font-mono text-[11px]">
            {relativeTime(r.updatedAtMs)}
          </span>
        </button>
      ))}
    </div>
  )
}

function StatusDot({ status }: { status?: string }) {
  if (status === 'running' || status === 'needs_input') {
    return <span className="bg-pax size-2 shrink-0 rounded-full" aria-label={status} />
  }
  if (status === 'failed') {
    return <span className="bg-destructive size-2 shrink-0 rounded-full" aria-label="failed" />
  }
  return <IconClock className="text-muted-foreground size-4 shrink-0" />
}

function relativeTime(ms: number): string {
  if (!ms) return ''
  const diff = Date.now() - ms
  const min = Math.round(diff / 60000)
  if (min < 1) return 'just now'
  if (min < 60) return `${min}m ago`
  const hr = Math.round(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.round(hr / 24)
  return `${day}d ago`
}
