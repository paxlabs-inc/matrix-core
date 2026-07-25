'use client'

/**
 * NeoTimeline — the Timeline page: everything Neo has committed to its own
 * memory that is safe to surface, newest-first and searchable.
 *
 * Neo keeps a durable, typed memory across every conversation (preferences,
 * facts, decisions, events, and more). This page is the human-facing control
 * surface for that memory: browse it, search it by free-text recall, filter by
 * type, and manage it directly — turn durable memory on or off, correct or
 * delete a single record, export everything, and request a full erasure.
 *
 * Durable personalization is OFF by default and only populated after an
 * explicit opt-in (PRIV-01). When memory is off, this page explains what would
 * be stored and stays quiet — nothing durable is written from casual chat.
 *
 * Design system: full-page overlay separated from the app by background TONE
 * only (bg-background), cards on bg-card, the single Paxeer-blue accent on
 * interactive chrome, the surface's rounded brand font, no border strokes for
 * depth, no emojis / glow.
 */
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { toast } from 'sonner'
import {
  BrainIcon,
  Check,
  Clock,
  Download,
  Loader2,
  MatrixIcon,
  RotateCcw,
  Search,
  ShieldCheck,
  Trash2Icon,
  X,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import { Switch } from '@/components/ui/switch'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'
import {
  deleteMemory,
  editMemory,
  exportMemories,
  getMemoryConsent,
  listMemoryTypeCounts,
  requestDeleteAll,
  searchMemories,
  setMemoryConsent,
  type DeleteAllUnavailable,
  type MemoryConsentResponse,
  type MemoryEntry,
} from '@/lib/api/memory'
import { NeoIllustration } from '@/components/matrix/neo/neo-illustration'

/** Friendly, human labels for the cortex type names. */
const TYPE_LABEL: Record<string, string> = {
  Identity: 'Identity',
  Fact: 'Facts',
  Preference: 'Preferences',
  Belief: 'Decisions',
  Event: 'Events',
  Goal: 'Goals',
  Constraint: 'Rules',
  Capability: 'Skills',
  Pattern: 'Patterns',
}

function typeLabel(t: string): string {
  return TYPE_LABEL[t] ?? t
}

export function NeoTimeline({ open, onClose }: { open: boolean; onClose: () => void }) {
  const reduce = useReducedMotion()
  const [query, setQuery] = useState('')
  const [activeType, setActiveType] = useState<string | null>(null)
  const [asOf, setAsOf] = useState<string>('')
  const [timeTravel, setTimeTravel] = useState(false)
  const [items, setItems] = useState<MemoryEntry[]>([])
  const [typeCounts, setTypeCounts] = useState<{ type: string; count: number }[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [consent, setConsent] = useState<MemoryConsentResponse | null>(null)
  const [consentBusy, setConsentBusy] = useState(false)
  const [refresh, setRefresh] = useState(0)
  const [deleteAllOpen, setDeleteAllOpen] = useState(false)
  const [deleteAllReceipt, setDeleteAllReceipt] = useState<DeleteAllUnavailable | null>(null)
  const [deleteAllBusy, setDeleteAllBusy] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  // Load the type chips once per open (counts are cheap + stable within a view).
  useEffect(() => {
    if (!open) return
    let alive = true
    listMemoryTypeCounts()
      .then((tc) => {
        if (alive) setTypeCounts(tc.filter((t) => t.count > 0))
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [open, refresh])

  // Load the durable-memory consent state and its pre-opt-in notice on open.
  useEffect(() => {
    if (!open) return
    let alive = true
    getMemoryConsent()
      .then((res) => {
        if (alive) setConsent(res)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [open])

  // Query memories on open and whenever the search text / type filter changes.
  // Debounced so typing doesn't storm the endpoint.
  useEffect(() => {
    if (!open) return
    let alive = true
    const ctrl = new AbortController()
    const run = () => {
      setLoading(true)
      setError(null)
      searchMemories({
        near: query.trim() || undefined,
        types: activeType ? [activeType] : undefined,
        asOf: timeTravel && asOf ? new Date(asOf).toISOString() : undefined,
        limit: 120,
        signal: ctrl.signal,
      })
        .then((res) => {
          if (alive) setItems(res)
        })
        .catch((e: unknown) => {
          if (!alive) return
          // An aborted request is expected on rapid retype — never an error.
          if ((e as { name?: string })?.name === 'AbortError') return
          setError('Could not load memories right now.')
          setItems([])
        })
        .finally(() => {
          if (alive) setLoading(false)
        })
    }
    const t = setTimeout(run, query.trim() ? 250 : 0)
    return () => {
      alive = false
      ctrl.abort()
      clearTimeout(t)
    }
  }, [open, query, activeType, timeTravel, asOf, refresh])

  // Focus the search field on open; Escape closes.
  useEffect(() => {
    if (!open) return
    const id = requestAnimationFrame(() => inputRef.current?.focus())
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => {
      cancelAnimationFrame(id)
      window.removeEventListener('keydown', onKey)
    }
  }, [open, onClose])

  const reset = useCallback(() => {
    setQuery('')
    setActiveType(null)
    setAsOf('')
    setTimeTravel(false)
  }, [])

  const bump = useCallback(() => setRefresh((n) => n + 1), [])

  const toggleConsent = useCallback((enabled: boolean) => {
    setConsentBusy(true)
    setMemoryConsent(enabled)
      .then((res) => {
        setConsent(res)
        toast.success(enabled ? 'Durable memory is on.' : 'Durable memory is off.')
      })
      .catch(() => toast.error('Could not update your memory setting.'))
      .finally(() => setConsentBusy(false))
  }, [])

  const handleDelete = useCallback(
    (m: MemoryEntry) => {
      setItems((prev) => prev.filter((x) => x.uri !== m.uri))
      deleteMemory(m.uri)
        .then(() => toast.success('Memory deleted.'))
        .catch(() => {
          toast.error('Could not delete that memory.')
          bump()
        })
    },
    [bump],
  )

  const handleEdit = useCallback(
    (m: MemoryEntry, text: string) => {
      const trimmed = text.trim()
      const current = m.form_medium || m.form_short || ''
      if (!trimmed || trimmed === current) return
      setItems((prev) => prev.map((x) => (x.uri === m.uri ? { ...x, form_medium: trimmed } : x)))
      editMemory(m.uri, m.type, trimmed)
        .then(() => {
          toast.success('Memory updated.')
          bump()
        })
        .catch(() => {
          toast.error('Could not update that memory.')
          bump()
        })
    },
    [bump],
  )

  const handleExport = useCallback(() => {
    exportMemories()
      .then((doc) => {
        const blob = new Blob([JSON.stringify(doc, null, 2)], { type: 'application/json' })
        const url = URL.createObjectURL(blob)
        const a = document.createElement('a')
        a.href = url
        a.download = `neo-memory-${new Date().toISOString().slice(0, 10)}.json`
        document.body.appendChild(a)
        a.click()
        a.remove()
        URL.revokeObjectURL(url)
      })
      .catch(() => toast.error('Could not export your memories.'))
  }, [])

  const runDeleteAll = useCallback(() => {
    setDeleteAllBusy(true)
    setDeleteAllReceipt(null)
    requestDeleteAll()
      .then((r) => {
        setDeleteAllReceipt(r)
        bump()
      })
      .catch(() => toast.error('Could not complete erasure.'))
      .finally(() => setDeleteAllBusy(false))
  }, [bump])

  const memoryEnabled = consent?.consent.enabled ?? false

  return (
    <AnimatePresence>
      {open ? (
        <motion.div
          initial={reduce ? false : { opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={reduce ? { opacity: 0 } : { opacity: 0 }}
          transition={{ duration: 0.2 }}
          className="bg-background fixed inset-0 z-50 flex flex-col"
          role="dialog"
          aria-modal="true"
          aria-label="Timeline — Neo's memory"
        >
          {/* header */}
          <div className="flex shrink-0 items-center gap-3 px-4 py-4 sm:px-6">
            <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
              <BrainIcon className="size-5" />
            </span>
            <div className="min-w-0 flex-1">
              <h1 className="text-foreground text-lg font-bold tracking-tight">Timeline</h1>
              <p className="text-muted-foreground text-xs">
                What Neo remembers across your conversations — yours to manage
              </p>
            </div>
            {memoryEnabled ? (
              <button
                type="button"
                onClick={handleExport}
                aria-label="Export all memories"
                className="text-muted-foreground hover:bg-muted hover:text-foreground flex h-9 shrink-0 items-center gap-1.5 rounded-full px-3 text-xs font-medium transition"
              >
                <Download className="size-4" />
                Export
              </button>
            ) : null}
            <button
              type="button"
              onClick={onClose}
              aria-label="Close timeline"
              className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 shrink-0 place-items-center rounded-full transition"
            >
              <X className="size-5" />
            </button>
          </div>

          {/* consent — durable memory is opt-in and off by default (PRIV-01) */}
          <div className="mx-auto w-full max-w-3xl shrink-0 px-4 pb-3 sm:px-6">
            <ConsentPanel consent={consent} busy={consentBusy} onToggle={toggleConsent} />
          </div>

          {/* search + filters */}
          <div className="mx-auto flex w-full max-w-3xl shrink-0 flex-col gap-3 px-4 pb-3 sm:px-6">
            <div className="bg-card flex items-center gap-2.5 rounded-xl px-3.5 py-2.5">
              <Search className="text-muted-foreground size-[1.05rem] shrink-0" />
              <input
                ref={inputRef}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search memories…"
                className="text-foreground placeholder:text-muted-foreground/70 min-w-0 flex-1 bg-transparent text-sm outline-none"
              />
              {query ? (
                <button
                  type="button"
                  onClick={() => setQuery('')}
                  aria-label="Clear search"
                  className="text-muted-foreground hover:text-foreground grid size-6 place-items-center rounded-full transition"
                >
                  <X className="size-4" />
                </button>
              ) : null}
            </div>

            {/* type filter chips — data-driven from /memory/types */}
            <div className="flex flex-wrap items-center gap-1.5">
              <Chip active={activeType === null} onClick={() => setActiveType(null)}>
                All
              </Chip>
              {typeCounts.map((tc) => (
                <Chip
                  key={tc.type}
                  active={activeType === tc.type}
                  onClick={() => setActiveType((prev) => (prev === tc.type ? null : tc.type))}
                >
                  {typeLabel(tc.type)}
                  <span className="text-muted-foreground/70 ml-1 font-mono text-[0.65rem]">
                    {tc.count}
                  </span>
                </Chip>
              ))}
            </div>

            {/* bi-temporal time-travel: query what Neo knew at a past instant */}
            <div className="flex items-center gap-2">
              <button
                type="button"
                onClick={() => setTimeTravel((v) => !v)}
                className={
                  timeTravel
                    ? 'bg-primary/15 text-primary flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors'
                    : 'text-muted-foreground hover:text-foreground flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors'
                }
              >
                <RotateCcw className="size-3.5" />
                Time travel
              </button>
              {timeTravel && (
                <input
                  type="datetime-local"
                  value={asOf}
                  onChange={(e) => setAsOf(e.target.value)}
                  className="bg-card text-foreground border-muted rounded-lg px-2.5 py-1.5 font-mono text-xs outline-none"
                  max={new Date().toISOString().slice(0, 16)}
                />
              )}
              {timeTravel && asOf && (
                <span className="text-muted-foreground text-[0.68rem]">
                  Showing what Neo knew at {new Date(asOf).toLocaleString()}
                </span>
              )}
            </div>
          </div>

          {/* body */}
          <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 sm:px-6">
            <div className="mx-auto w-full max-w-3xl">
              {loading && items.length === 0 ? (
                <MemorySkeleton />
              ) : error ? (
                <EmptyState title="Memory is unavailable" body={error} />
              ) : items.length === 0 ? (
                <EmptyState
                  title={query || activeType ? 'No matching memories' : 'No memories yet'}
                  body={
                    query || activeType
                      ? 'Try a different search or clear the filters.'
                      : 'Memories form as you work with Neo — the preferences, facts, and decisions it learns will appear here.'
                  }
                  onReset={query || activeType ? reset : undefined}
                />
              ) : (
                <>
                  <ul className="flex flex-col gap-2">
                    {items.map((m) => (
                      <MemoryRow
                        key={m.uri}
                        memory={m}
                        editable={!timeTravel}
                        onEdit={handleEdit}
                        onDelete={handleDelete}
                      />
                    ))}
                  </ul>
                  {!timeTravel ? (
                    <div className="mt-6 flex flex-col items-start gap-2">
                      <button
                        type="button"
                        onClick={() => {
                          setDeleteAllReceipt(null)
                          setDeleteAllOpen(true)
                        }}
                        className="text-destructive/80 hover:text-destructive flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs font-medium transition-colors"
                      >
                        <Trash2Icon className="size-3.5" />
                        Delete all memories
                      </button>
                    </div>
                  ) : null}
                </>
              )}
            </div>
          </div>

          <DeleteAllDialog
            open={deleteAllOpen}
            onOpenChange={setDeleteAllOpen}
            busy={deleteAllBusy}
            receipt={deleteAllReceipt}
            onConfirm={runDeleteAll}
          />
        </motion.div>
      ) : null}
    </AnimatePresence>
  )
}

function Chip({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'flex items-center rounded-full px-3 py-1.5 text-xs font-medium transition-colors',
        active
          ? 'bg-primary text-primary-foreground'
          : 'bg-card text-muted-foreground hover:text-foreground',
      )}
    >
      {children}
    </button>
  )
}

function MemoryRow({
  memory,
  editable,
  onEdit,
  onDelete,
}: {
  memory: MemoryEntry
  editable: boolean
  onEdit: (m: MemoryEntry, text: string) => void
  onDelete: (m: MemoryEntry) => void
}) {
  const text = memory.form_medium || memory.form_short || '(no summary)'
  const when = memory.updated_at || memory.created_at
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(text)
  const [confirmingDelete, setConfirmingDelete] = useState(false)

  const startEdit = () => {
    setDraft(memory.form_medium || memory.form_short || '')
    setEditing(true)
  }
  const save = () => {
    onEdit(memory, draft)
    setEditing(false)
  }

  return (
    <li className="bg-card group flex flex-col gap-1.5 rounded-xl p-3.5">
      <div className="flex items-center gap-2">
        <span className="bg-muted text-muted-foreground rounded-md px-2 py-0.5 text-[0.65rem] font-semibold tracking-wide uppercase">
          {typeLabel(memory.type)}
        </span>
        {when ? (
          <span className="text-muted-foreground/70 flex items-center gap-1 text-[0.7rem]">
            <Clock className="size-3" />
            {relativeTime(when)}
          </span>
        ) : null}
        {editable && !editing ? (
          <div className="ml-auto flex items-center gap-1">
            <button
              type="button"
              onClick={startEdit}
              aria-label="Edit memory"
              className="text-muted-foreground/70 hover:text-foreground hover:bg-muted grid size-7 place-items-center rounded-full transition"
            >
              <MatrixIcon name="edit" className="size-3.5" />
            </button>
            <button
              type="button"
              onClick={() => (confirmingDelete ? onDelete(memory) : setConfirmingDelete(true))}
              onBlur={() => setConfirmingDelete(false)}
              aria-label={confirmingDelete ? 'Confirm delete' : 'Delete memory'}
              className={cn(
                'grid size-7 place-items-center rounded-full transition',
                confirmingDelete
                  ? 'text-destructive bg-destructive/10'
                  : 'text-muted-foreground/70 hover:text-destructive hover:bg-muted',
              )}
            >
              {confirmingDelete ? (
                <Check className="size-3.5" />
              ) : (
                <Trash2Icon className="size-3.5" />
              )}
            </button>
          </div>
        ) : null}
      </div>
      {editing ? (
        <div className="flex flex-col gap-2">
          <textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            rows={3}
            autoFocus
            className="bg-background text-foreground w-full resize-y rounded-lg px-3 py-2 text-[0.88rem] leading-relaxed outline-none"
          />
          <div className="flex items-center gap-2">
            <Button size="sm" onClick={save}>
              <Check className="size-4" />
              Save
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <p className="text-foreground text-[0.88rem] leading-relaxed [overflow-wrap:anywhere]">
          {text}
        </p>
      )}
      {memory.tags && memory.tags.length > 0 ? (
        <div className="mt-0.5 flex flex-wrap gap-1">
          {memory.tags.slice(0, 6).map((tag) => (
            <span
              key={tag}
              className="text-muted-foreground/80 bg-muted/60 rounded px-1.5 py-0.5 font-mono text-[0.65rem]"
            >
              {tag}
            </span>
          ))}
        </div>
      ) : null}
    </li>
  )
}

/**
 * ConsentPanel — durable-memory opt-in. Off by default; explains what is
 * stored before the first write and lets the user turn it on or off. When off,
 * nothing durable is written from casual chat.
 */
function ConsentPanel({
  consent,
  busy,
  onToggle,
}: {
  consent: MemoryConsentResponse | null
  busy: boolean
  onToggle: (enabled: boolean) => void
}) {
  if (!consent) return null
  const enabled = consent.consent.enabled
  return (
    <div className="bg-card rounded-xl p-4">
      <label className="flex cursor-pointer items-start gap-3">
        <ShieldCheck className="text-primary mt-0.5 size-4 shrink-0" />
        <div className="min-w-0 flex-1">
          <p className="text-foreground text-sm font-medium">Remember what I share</p>
          <p className="text-muted-foreground mt-0.5 text-xs [overflow-wrap:anywhere]">
            {consent.notice}
          </p>
          {!enabled ? (
            <p className="text-muted-foreground/80 mt-1 text-[0.7rem]">
              Durable memory is off. Nothing from your conversations is kept until you turn it on.
            </p>
          ) : consent.consent.existing_data ? (
            <p className="text-muted-foreground/80 mt-1 text-[0.7rem] [overflow-wrap:anywhere]">
              {consent.consent.existing_data}
            </p>
          ) : null}
        </div>
        <Switch
          checked={enabled}
          onCheckedChange={onToggle}
          disabled={busy}
          aria-label="Remember what I share"
        />
      </label>
    </div>
  )
}

/**
 * DeleteAllDialog — full erasure. Complete cryptographic erasure with a
 * deletion receipt is gated on the ORACLE pipeline; until it ships the server
 * refuses fail-closed and this dialog shows the prerequisite plainly rather
 * than partially deleting.
 */
function DeleteAllDialog({
  open,
  onOpenChange,
  busy,
  receipt,
  onConfirm,
}: {
  open: boolean
  onOpenChange: (v: boolean) => void
  busy: boolean
  receipt: DeleteAllUnavailable | null
  onConfirm: () => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Delete all memories</DialogTitle>
          <DialogDescription>
            This permanently erases everything Neo has remembered about you across every
            conversation — from storage, search, and recall.
          </DialogDescription>
        </DialogHeader>

        {receipt ? (
          <div className="bg-card text-muted-foreground rounded-lg p-3 text-sm [overflow-wrap:anywhere]">
            <p className="text-foreground font-medium">Not available yet</p>
            <p className="mt-1 text-xs">{receipt.error}</p>
            {receipt.alternative ? <p className="mt-1 text-xs">{receipt.alternative}</p> : null}
          </div>
        ) : null}

        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="sm"
            className="text-destructive"
            onClick={onConfirm}
            disabled={busy || !!receipt}
          >
            {busy ? <Loader2 className="size-4 animate-spin" /> : <Trash2Icon className="size-4" />}
            Delete everything
          </Button>
          <Button variant="secondary" size="sm" onClick={() => onOpenChange(false)}>
            Close
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}

function EmptyState({
  title,
  body,
  onReset,
}: {
  title: string
  body: string
  onReset?: () => void
}) {
  return (
    <div className="flex flex-col items-center gap-4 py-16 text-center">
      <NeoIllustration art="timeline" width={190} />
      <div className="flex flex-col gap-1">
        <p className="text-foreground text-base font-bold">{title}</p>
        <p className="text-muted-foreground mx-auto max-w-sm text-sm">{body}</p>
      </div>
      {onReset ? (
        <button
          type="button"
          onClick={onReset}
          className="text-primary hover:bg-primary/10 rounded-full px-3 py-1.5 text-sm font-medium transition-colors"
        >
          Clear filters
        </button>
      ) : null}
    </div>
  )
}

function MemorySkeleton() {
  return (
    <ul className="flex flex-col gap-2">
      {Array.from({ length: 6 }).map((_, i) => (
        <li key={i} className="bg-card flex flex-col gap-2 rounded-xl p-3.5">
          <div className="bg-muted h-3.5 w-24 rounded" />
          <div className="bg-muted h-3 w-full rounded" />
          <div className="bg-muted h-3 w-2/3 rounded" />
        </li>
      ))}
    </ul>
  )
}

/** Compact "2h ago" style relative timestamp. Falls back to a date. */
function relativeTime(iso: string): string {
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const diff = Date.now() - t
  const sec = Math.round(diff / 1000)
  if (sec < 60) return 'just now'
  const min = Math.round(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.round(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.round(hr / 24)
  if (day < 7) return `${day}d ago`
  return new Date(t).toLocaleDateString()
}
