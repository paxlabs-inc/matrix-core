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
 * Paxeer-blue accent, no border strokes for depth, no emojis / glow.
 */
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { MessageSquare, Plus, Search, X } from '@/lib/matrix-icons'
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
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  conversations: ConversationSummary[]
  activeConversationId?: string | null
  onSelect: (id: string) => void
  onNewChat: () => void
}) {
  const reduce = useReducedMotion()
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)

  // Reset the query + selection each time the palette opens; focus the input.
  useEffect(() => {
    if (!open) return
    setQuery('')
    setCursor(0)
    const id = requestAnimationFrame(() => inputRef.current?.focus())
    return () => cancelAnimationFrame(id)
  }, [open])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return conversations
    return conversations.filter(
      (c) =>
        (c.title || '').toLowerCase().includes(q) || (c.preview || '').toLowerCase().includes(q),
    )
  }, [conversations, query])

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
    <AnimatePresence>
      {open ? (
        <motion.div
          initial={reduce ? false : { opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: 0.15 }}
          className="fixed inset-0 z-50 flex items-start justify-center px-4 pt-[12vh]"
          onKeyDown={onKeyDown}
        >
          {/* scrim — tone only, click to dismiss */}
          <div
            className="bg-background/70 absolute inset-0 backdrop-blur-sm"
            onClick={() => onOpenChange(false)}
            aria-hidden
          />

          <motion.div
            initial={reduce ? false : { opacity: 0, y: -8, scale: 0.99 }}
            animate={{ opacity: 1, y: 0, scale: 1 }}
            exit={reduce ? { opacity: 0 } : { opacity: 0, y: -8, scale: 0.99 }}
            transition={{ duration: 0.18, ease: [0.32, 0.72, 0, 1] }}
            role="dialog"
            aria-modal="true"
            aria-label="Search conversations"
            className="bg-popover relative flex max-h-[70vh] w-full max-w-xl flex-col overflow-hidden rounded-2xl"
          >
            {/* search bar */}
            <div className="flex items-center gap-3 px-4 py-3.5">
              <Search className="text-muted-foreground size-[1.1rem] shrink-0" />
              <input
                ref={inputRef}
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value)
                  setCursor(0)
                }}
                placeholder="Search conversations…"
                className="text-foreground placeholder:text-muted-foreground/70 min-w-0 flex-1 bg-transparent text-sm outline-none"
              />
              <button
                type="button"
                onClick={() => onOpenChange(false)}
                aria-label="Close search"
                className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-7 shrink-0 place-items-center rounded-full transition"
              >
                <X className="size-4" />
              </button>
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
                New chat
              </button>

              {flat.length === 0 ? (
                <div className="text-muted-foreground flex flex-col items-center gap-2 px-4 py-10 text-center text-sm">
                  <MessageSquare className="size-5 opacity-60" />
                  <p>{query ? 'No conversations match your search.' : 'No conversations yet.'}</p>
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
                          <li key={c.conversation_id}>
                            <button
                              type="button"
                              onMouseEnter={() => setCursor(flatIndex)}
                              onClick={() => choose(c.conversation_id)}
                              className={cn(
                                'flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left transition-colors',
                                on ? 'bg-muted' : 'hover:bg-muted/60',
                              )}
                            >
                              <MessageSquare
                                className={cn(
                                  'size-4 shrink-0',
                                  active ? 'text-primary' : 'text-muted-foreground/70',
                                )}
                              />
                              <span className="min-w-0 flex-1">
                                <span className="text-foreground block truncate text-sm">
                                  {c.title || 'Untitled chat'}
                                </span>
                                {c.preview ? (
                                  <span className="text-muted-foreground block truncate text-xs">
                                    {c.preview}
                                  </span>
                                ) : null}
                              </span>
                            </button>
                          </li>
                        )
                      })}
                    </ul>
                  </div>
                ))
              )}
            </div>
          </motion.div>
        </motion.div>
      ) : null}
    </AnimatePresence>
  )
}
