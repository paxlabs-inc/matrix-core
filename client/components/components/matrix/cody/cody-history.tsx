'use client'

/**
 * History (NEO-WORKBENCH req 1.3) — the project's Neo conversations, newest
 * first, straight from the server-backed conversation list (already scoped
 * to the active project by the useChat reducer). Opening an entry reopens
 * the thread and rebuilds the workbench from its durable trace.
 */
import { useMemo } from 'react'

import { IconClock } from '@/components/matrix/cody/icons'
import type { ConversationSummary } from '@/lib/api/conversations'

export function CodyHistory({
  conversations,
  onOpen,
}: {
  conversations: ConversationSummary[]
  onOpen: (convID: string) => void
}) {
  const rows = useMemo(
    () =>
      [...conversations].sort(
        (a, b) => (Date.parse(b.updated) || 0) - (Date.parse(a.updated) || 0),
      ),
    [conversations],
  )

  if (rows.length === 0) {
    return (
      <div className="text-muted-foreground m-auto flex h-full flex-col items-center justify-center gap-2 p-8 text-sm">
        <IconClock className="size-6 opacity-60" />
        <span>Nothing here yet. Ask Neo to build something from the Workspace.</span>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2 p-4">
      {rows.map((r) => (
        <button
          key={r.conversation_id}
          type="button"
          onClick={() => onOpen(r.conversation_id)}
          className="bg-surface-secondary hover:bg-surface-hover flex items-center gap-3 rounded-lg px-4 py-3 text-left transition-colors"
        >
          <IconClock className="text-muted-foreground size-4 shrink-0" />
          <span className="min-w-0 flex-1 truncate text-sm">{r.title || r.conversation_id}</span>
          {r.preview ? (
            <span className="text-muted-foreground hidden max-w-[40%] truncate text-xs lg:block">
              {r.preview}
            </span>
          ) : null}
          <span className="text-muted-foreground ml-auto shrink-0 font-mono text-[11px]">
            {relativeTime(Date.parse(r.updated) || 0)}
          </span>
        </button>
      ))}
    </div>
  )
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
