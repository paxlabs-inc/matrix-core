'use client'

/**
 * History (NEO-WORKBENCH req 1.3) — the project's Neo conversations, newest
 * first, straight from the server-backed conversation list (already scoped
 * to the active project by the useChat reducer). Opening an entry reopens
 * the thread and rebuilds the workbench from its durable trace.
 */
import { useMemo, useState } from 'react'

import { IconClock, IconMore, IconSettings, IconTrash } from '@/components/matrix/cody/icons'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import type { ConversationSummary } from '@/lib/api/conversations'

export function CodyHistory({
  conversations,
  onOpen,
  onRename,
  onArchive,
  onDelete,
}: {
  conversations: ConversationSummary[]
  onOpen: (convID: string) => void
  onRename?: (convID: string, title: string) => void
  onArchive?: (convID: string, archived: boolean) => void
  onDelete?: (convID: string) => void
}) {
  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null)
  const rows = useMemo(
    () =>
      [...conversations].sort(
        (a, b) => (Date.parse(b.updated) || 0) - (Date.parse(a.updated) || 0),
      ),
    [conversations],
  )
  const commitRename = (convID: string) => {
    const title = renameDraft.trim()
    setRenamingId(null)
    if (title) onRename?.(convID, title)
  }

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
      {rows.map((r) => {
        const deleting = confirmDeleteId === r.conversation_id
        return (
          <div
            key={r.conversation_id}
            className="bg-surface-secondary rounded-lg px-3 py-2 transition-colors"
          >
            {deleting ? (
              <div className="flex min-h-11 flex-wrap items-center gap-3 px-1">
                <p className="min-w-60 flex-1 text-sm">
                  Delete turns and traces? Memories and media stay under Memory controls.
                </p>
                <button
                  type="button"
                  onClick={() => setConfirmDeleteId(null)}
                  className="text-muted-foreground hover:bg-surface-hover min-h-11 rounded-md px-3 text-sm"
                >
                  Cancel
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setConfirmDeleteId(null)
                    onDelete?.(r.conversation_id)
                  }}
                  className="text-destructive hover:bg-destructive/10 min-h-11 rounded-md px-3 text-sm"
                >
                  Delete
                </button>
              </div>
            ) : renamingId === r.conversation_id ? (
              <input
                autoFocus
                value={renameDraft}
                onChange={(event) => setRenameDraft(event.target.value)}
                onBlur={() => commitRename(r.conversation_id)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') commitRename(r.conversation_id)
                  if (event.key === 'Escape') setRenamingId(null)
                }}
                aria-label="Conversation name"
                className="bg-surface-hover text-foreground h-11 w-full rounded-md px-3 text-sm outline-none"
              />
            ) : (
              <div className="flex min-h-11 items-center gap-2">
                <button
                  type="button"
                  onClick={() => onOpen(r.conversation_id)}
                  onDoubleClick={(event) => {
                    if (!onRename) return
                    event.preventDefault()
                    setRenameDraft(r.title || '')
                    setRenamingId(r.conversation_id)
                  }}
                  className="hover:bg-surface-hover flex min-h-11 min-w-0 flex-1 items-center gap-3 rounded-md px-1 text-left transition-colors"
                >
                  <IconClock className="text-muted-foreground size-4 shrink-0" />
                  <span className="min-w-0 flex-1 truncate text-sm">
                    {r.title || r.conversation_id}
                  </span>
                  {r.archived ? (
                    <span className="bg-surface-hover text-muted-foreground rounded-full px-2 py-1 text-[11px]">
                      Archived
                    </span>
                  ) : null}
                  {r.forked_from ? (
                    <span className="text-muted-foreground hidden text-[11px] xl:inline">Fork</span>
                  ) : null}
                  {r.preview ? (
                    <span className="text-muted-foreground hidden max-w-[40%] truncate text-xs lg:block">
                      {r.preview}
                    </span>
                  ) : null}
                  <span className="text-muted-foreground shrink-0 font-mono text-[11px]">
                    {relativeTime(Date.parse(r.updated) || 0)}
                  </span>
                </button>
                {onRename || onArchive || onDelete ? (
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <button
                        type="button"
                        aria-label={`Manage ${r.title || 'conversation'}`}
                        className="text-muted-foreground hover:bg-surface-hover hover:text-foreground grid size-11 shrink-0 place-items-center rounded-md"
                      >
                        <IconMore className="size-4" />
                      </button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      {onRename ? (
                        <DropdownMenuItem
                          onSelect={() => {
                            setRenameDraft(r.title || '')
                            setRenamingId(r.conversation_id)
                          }}
                        >
                          <IconSettings />
                          Rename
                        </DropdownMenuItem>
                      ) : null}
                      {onArchive ? (
                        <DropdownMenuItem
                          onSelect={() => onArchive(r.conversation_id, !r.archived)}
                        >
                          <IconClock />
                          {r.archived ? 'Restore' : 'Archive'}
                        </DropdownMenuItem>
                      ) : null}
                      {onDelete ? (
                        <DropdownMenuItem
                          variant="destructive"
                          onSelect={() => setConfirmDeleteId(r.conversation_id)}
                        >
                          <IconTrash />
                          Delete…
                        </DropdownMenuItem>
                      ) : null}
                    </DropdownMenuContent>
                  </DropdownMenu>
                ) : null}
              </div>
            )}
          </div>
        )
      })}
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
