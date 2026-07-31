'use client'

/**
 * NeoChatSearch — a proper search over your conversations.
 *
 * A command-palette-style modal: a real search bar that filters every persisted
 * thread by title + preview as you type, results grouped by recency, keyboard
 * navigable (↑/↓ to move, Enter to open, Esc to close), with "New chat" always
 * one keystroke away. Selecting a thread reopens it in the chat surface.
 *
 * Data is the durable conversation list the daemon persists (GET /conversations)
 * — the same list the sidebar shows; this is the focused way to find one.
 *
 * Design system: centered overlay separated by background TONE only (a
 * translucent scrim over the app, a bg-popover panel above it), single
 * Matrix Sage accent, no border strokes for depth, no emojis / glow.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { Dialog } from '@astryxdesign/core/Dialog'
import { Button } from '@astryxdesign/core/Button'
import { TextInput } from '@astryxdesign/core/TextInput'
import { ToggleButton, ToggleButtonGroup } from '@astryxdesign/core/ToggleButton'
import { Heading, Text } from '@astryxdesign/core/Text'
import {
  Check,
  EyeOffIcon,
  MessageSquare,
  MoreHorizontal,
  Plus,
  RotateCcw,
  Search,
  Trash2Icon,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { ConversationSummary } from '@/lib/api/conversations'

interface Group {
  label: string
  items: ConversationSummary[]
}

/** Bucket conversations into Today / Yesterday / This week / Earlier. */
function groupByRecency(items: ConversationSummary[]): Group[] {
  const now = new Date()
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime()
  const dayMs = 86_400_000
  const buckets: Record<string, ConversationSummary[]> = {
    Today: [],
    Yesterday: [],
    'This week': [],
    Earlier: [],
  }
  for (const c of items) {
    const t = Date.parse(c.updated)
    if (Number.isNaN(t)) {
      buckets.Earlier.push(c)
      continue
    }
    if (t >= startOfToday) buckets.Today.push(c)
    else if (t >= startOfToday - dayMs) buckets.Yesterday.push(c)
    else if (t >= startOfToday - 7 * dayMs) buckets['This week'].push(c)
    else buckets.Earlier.push(c)
  }
  return Object.entries(buckets)
    .filter(([, v]) => v.length > 0)
    .map(([label, v]) => ({ label, items: v }))
}

export function NeoChatSearch({
  open,
  onOpenChange,
  conversations,
  activeConversationId,
  onSelect,
  onNewChat,
  onArchive,
  onRename,
  onDelete,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  conversations: ConversationSummary[]
  activeConversationId?: string | null
  onSelect: (id: string) => void
  onNewChat: () => void
  onArchive?: (id: string, archived: boolean) => void
  onRename?: (id: string, title: string) => void
  onDelete?: (id: string) => void
}) {
  const reduce = useReducedMotion()
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const [view, setView] = useState<'all' | 'active' | 'archived'>('all')
  const [managingId, setManagingId] = useState<string | null>(null)
  const [renamingId, setRenamingId] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  // Reset the query + selection each time the palette opens; focus the input.
  useEffect(() => {
    if (!open) return
    setQuery('')
    setCursor(0)
    setManagingId(null)
    setRenamingId(null)
    setConfirmDeleteId(null)
    const id = requestAnimationFrame(() => inputRef.current?.focus())
    return () => cancelAnimationFrame(id)
  }, [open])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return conversations.filter((c) => {
      if (view === 'active' && c.archived) return false
      if (view === 'archived' && !c.archived) return false
      if (!q) return true
      return (
        (c.title || '').toLowerCase().includes(q) || (c.preview || '').toLowerCase().includes(q)
      )
    })
  }, [conversations, query, view])

  const groups = useMemo(() => groupByRecency(filtered), [filtered])

  // A flat, ordered view of the visible rows for keyboard navigation.
  const flat = useMemo(() => groups.flatMap((g) => g.items), [groups])

  useEffect(() => {
    if (cursor > flat.length - 1) setCursor(Math.max(0, flat.length - 1))
  }, [flat.length, cursor])

  const choose = (id: string) => {
    onSelect(id)
    onOpenChange(false)
  }

  const beginRename = (conversation: ConversationSummary) => {
    setRenameDraft(conversation.title || '')
    setRenamingId(conversation.conversation_id)
    setManagingId(null)
  }

  const commitRename = (id: string) => {
    const next = renameDraft.trim()
    setRenamingId(null)
    if (next) onRename?.(id, next)
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setCursor((c) => Math.min(flat.length - 1, c + 1))
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setCursor((c) => Math.max(0, c - 1))
    } else if (e.key === 'Enter') {
      e.preventDefault()
      const pick = flat[cursor]
      if (pick) choose(pick.conversation_id)
    } else if (e.key === 'Escape') {
      e.preventDefault()
      onOpenChange(false)
    }
  }

  return (
    <Dialog
      isOpen={open}
      onOpenChange={onOpenChange}
      purpose="info"
      width="min(calc(100vw - 2rem), 42rem)"
      maxHeight="76vh"
      padding={0}
      aria-label="All tasks"
    >
      <motion.div
        initial={reduce ? false : { opacity: 0, y: -8, scale: 0.99 }}
        animate={{ opacity: 1, y: 0, scale: 1 }}
        exit={reduce ? { opacity: 0 } : { opacity: 0, y: -8, scale: 0.99 }}
        transition={{ duration: 0.18, ease: [0.32, 0.72, 0, 1] }}
        className="bg-popover relative flex max-h-[76vh] w-full flex-col overflow-hidden"
        onKeyDown={onKeyDown}
      >
        <div className="flex items-start gap-3 px-4 pt-4 pb-3">
          <div className="min-w-0 flex-1">
            <Heading level={2}>All tasks</Heading>
            <Text type="supporting" color="secondary" display="block">
              Find, rename, archive, restore, or remove a conversation.
            </Text>
          </div>
          <Button
            label="Close search"
            variant="ghost"
            size="sm"
            icon={<X className="size-4" />}
            isIconOnly
            onClick={() => onOpenChange(false)}
          />
        </div>

        <div className="px-4 pb-3">
          <TextInput
            ref={inputRef}
            label="Search every task"
            isLabelHidden
            value={query}
            onChange={(value) => {
              setQuery(value)
              setCursor(0)
            }}
            placeholder="Search every task…"
            startIcon={<Search className="size-4" />}
            hasClear
            width="100%"
          />
          <div className="mt-2">
            <ToggleButtonGroup
              value={view}
              onChange={(value) => {
                if (value) setView(value as typeof view)
                setCursor(0)
              }}
              label="Task state"
              size="sm"
            >
              {(['all', 'active', 'archived'] as const).map((option) => (
                <ToggleButton
                  key={option}
                  value={option}
                  label={option[0].toUpperCase() + option.slice(1)}
                />
              ))}
            </ToggleButtonGroup>
          </div>
        </div>

        {/* results */}
        <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-2">
          {/* New chat — always available */}
          <button
            type="button"
            onClick={() => {
              onNewChat()
              onOpenChange(false)
            }}
            className="text-foreground hover:bg-muted flex w-full items-center gap-2.5 rounded-lg px-3 py-2.5 text-left text-sm font-medium transition-colors"
          >
            <span className="bg-primary/15 text-primary grid size-7 place-items-center rounded-lg">
              <Plus className="size-4" />
            </span>
            New task
          </button>

          {flat.length === 0 ? (
            <div className="text-muted-foreground flex flex-col items-center gap-2 px-4 py-10 text-center text-sm">
              <MessageSquare className="size-5 opacity-60" />
              <p>{query ? 'No tasks match your search.' : `No ${view} tasks yet.`}</p>
            </div>
          ) : (
            groups.map((group) => (
              <div key={group.label} className="mt-1">
                <p className="text-muted-foreground/70 px-3 pt-2 pb-1 text-[0.68rem] font-semibold tracking-wide uppercase">
                  {group.label}
                </p>
                <ul>
                  {group.items.map((c) => {
                    const flatIndex = flat.indexOf(c)
                    const on = flatIndex === cursor
                    const active = c.conversation_id === activeConversationId
                    return (
                      <li key={c.conversation_id} className="group/task">
                        {confirmDeleteId === c.conversation_id ? (
                          <div className="bg-destructive/10 rounded-xl px-3 py-3">
                            <p className="text-foreground text-sm font-medium">
                              Delete “{c.title || 'Untitled task'}”?
                            </p>
                            <p className="text-muted-foreground mt-1 text-xs">
                              Turns and traces are removed. Memory and media remain under their own
                              controls.
                            </p>
                            <div className="mt-3 flex justify-end gap-2">
                              <button
                                type="button"
                                onClick={() => setConfirmDeleteId(null)}
                                className="text-muted-foreground hover:text-foreground min-h-9 rounded-lg px-3 text-xs font-medium"
                              >
                                Cancel
                              </button>
                              <button
                                type="button"
                                onClick={() => {
                                  setConfirmDeleteId(null)
                                  onDelete?.(c.conversation_id)
                                }}
                                className="bg-destructive text-destructive-foreground min-h-9 rounded-lg px-3 text-xs font-medium"
                              >
                                Delete task
                              </button>
                            </div>
                          </div>
                        ) : renamingId === c.conversation_id ? (
                          <div className="bg-muted flex items-center gap-2 rounded-xl px-3 py-2">
                            <input
                              autoFocus
                              value={renameDraft}
                              onChange={(event) => setRenameDraft(event.target.value)}
                              onKeyDown={(event) => {
                                event.stopPropagation()
                                if (event.key === 'Enter') commitRename(c.conversation_id)
                                if (event.key === 'Escape') setRenamingId(null)
                              }}
                              aria-label="Task name"
                              className="text-foreground min-w-0 flex-1 bg-transparent text-sm outline-none"
                            />
                            <button
                              type="button"
                              onClick={() => commitRename(c.conversation_id)}
                              aria-label="Save task name"
                              className="text-primary grid size-8 place-items-center rounded-full"
                            >
                              <Check className="size-4" />
                            </button>
                          </div>
                        ) : (
                          <div
                            className={cn(
                              'rounded-xl transition-colors',
                              on ? 'bg-muted' : 'hover:bg-muted/60',
                            )}
                          >
                            <div className="flex items-center">
                              <button
                                type="button"
                                onMouseEnter={() => setCursor(flatIndex)}
                                onClick={() => choose(c.conversation_id)}
                                className="flex min-w-0 flex-1 items-center gap-2.5 px-3 py-2 text-left"
                              >
                                <MessageSquare
                                  className={cn(
                                    'size-4 shrink-0',
                                    active ? 'text-primary' : 'text-muted-foreground/70',
                                  )}
                                />
                                <span className="min-w-0 flex-1">
                                  <span className="text-foreground flex items-center gap-2 text-sm">
                                    <span className="truncate">{c.title || 'Untitled task'}</span>
                                    {c.archived ? (
                                      <span className="bg-background text-muted-foreground shrink-0 rounded px-1.5 py-0.5 text-[0.62rem] font-semibold uppercase">
                                        Archived
                                      </span>
                                    ) : null}
                                  </span>
                                  {c.preview ? (
                                    <span className="text-muted-foreground block truncate text-xs">
                                      {c.preview}
                                    </span>
                                  ) : null}
                                </span>
                              </button>
                              <button
                                type="button"
                                onClick={() =>
                                  setManagingId((current) =>
                                    current === c.conversation_id ? null : c.conversation_id,
                                  )
                                }
                                aria-label={`Manage ${c.title || 'Untitled task'}`}
                                aria-expanded={managingId === c.conversation_id}
                                className="text-muted-foreground hover:bg-background/60 hover:text-foreground mr-1 grid size-9 shrink-0 place-items-center rounded-lg"
                              >
                                <MoreHorizontal className="size-4" />
                              </button>
                            </div>
                            {managingId === c.conversation_id ? (
                              <div className="bg-background/60 mx-2 mb-2 flex flex-wrap items-center gap-1 rounded-lg p-1">
                                <TaskAction onClick={() => beginRename(c)}>Rename</TaskAction>
                                <TaskAction
                                  onClick={() => {
                                    setManagingId(null)
                                    onArchive?.(c.conversation_id, !c.archived)
                                  }}
                                >
                                  {c.archived ? (
                                    <RotateCcw className="size-3.5" />
                                  ) : (
                                    <EyeOffIcon className="size-3.5" />
                                  )}
                                  {c.archived ? 'Restore' : 'Archive'}
                                </TaskAction>
                                <TaskAction
                                  destructive
                                  onClick={() => {
                                    setManagingId(null)
                                    setConfirmDeleteId(c.conversation_id)
                                  }}
                                >
                                  <Trash2Icon className="size-3.5" />
                                  Delete
                                </TaskAction>
                              </div>
                            ) : null}
                          </div>
                        )}
                      </li>
                    )
                  })}
                </ul>
              </div>
            ))
          )}
        </div>
      </motion.div>
    </Dialog>
  )
}

function TaskAction({
  children,
  destructive = false,
  onClick,
}: {
  children: ReactNode
  destructive?: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'flex min-h-8 items-center gap-1.5 rounded-md px-2.5 text-xs font-medium transition-colors',
        destructive
          ? 'text-destructive hover:bg-destructive/10'
          : 'text-muted-foreground hover:bg-muted hover:text-foreground',
      )}
    >
      {children}
    </button>
  )
}
