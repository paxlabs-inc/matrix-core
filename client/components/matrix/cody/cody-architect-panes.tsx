'use client'

/**
 * Architect-only panes (task 6.4). Two surfaces the Architect tier adds on top
 * of the Engineer workspace:
 *
 *   - CodyTerminal — a bounded command runner over POST /workspace/exec. v1 is
 *     NOT a PTY: you type one command, it runs to completion (or times out),
 *     and the scrollback records the command, its output, exit code, and the
 *     wall-clock duration. Architect gating is the caller's job.
 *   - CodySpecViewer — the live .cody/spec viewer. Reads requirements.md and
 *     tasks.md via GET /workspace/file and renders them; tasks.md's markdown
 *     checkboxes are parsed into a read-only checklist with checked state. When
 *     the durable spec is absent (Prototype/Engineer projects never author one)
 *     it shows an honest empty state rather than an error.
 *
 * House rules: layers separate by background tone (no border strokes for
 * depth), single sage accent via the `pax` token, monospace for machine text.
 */
import { useCallback, useEffect, useRef, useState } from 'react'

import { ApiError } from '@/lib/api/client'
import { execCommand, getFile } from '@/lib/api/cody'
import type { ExecResult } from '@/lib/api/cody'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import {
  IconTerminal,
  IconAlertCircle,
  IconCheck,
  IconFileText,
} from '@/components/matrix/cody/icons'

// ── Terminal ──────────────────────────────────────────────────────────────────

interface TerminalEntry {
  cmd: string
  result?: ExecResult
  /** Wall-clock duration measured client-side; the exec route returns no timing. */
  durationMs?: number
  error?: string
  running: boolean
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)}s`
}

export function CodyTerminal({ projectID }: { projectID?: string }) {
  const [cmd, setCmd] = useState('')
  const [entries, setEntries] = useState<TerminalEntry[]>([])
  const [busy, setBusy] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [entries])

  const run = useCallback(async () => {
    const trimmed = cmd.trim()
    if (!trimmed || busy) return
    setBusy(true)
    setCmd('')
    const index = entries.length
    setEntries((prev) => [...prev, { cmd: trimmed, running: true }])
    const started = performance.now()
    try {
      const result = await execCommand(trimmed, { projectID })
      const durationMs = performance.now() - started
      setEntries((prev) =>
        prev.map((e, i) =>
          i === index ? { cmd: trimmed, result, durationMs, running: false } : e,
        ),
      )
    } catch (err) {
      const durationMs = performance.now() - started
      const message = err instanceof Error ? err.message : 'Command failed'
      setEntries((prev) =>
        prev.map((e, i) =>
          i === index ? { cmd: trimmed, error: message, durationMs, running: false } : e,
        ),
      )
    } finally {
      setBusy(false)
    }
  }, [cmd, busy, entries.length, projectID])

  return (
    <section className="bg-surface-secondary flex h-full min-h-0 flex-col gap-3 rounded-lg p-4">
      <div className="flex shrink-0 items-center gap-2">
        <IconTerminal className="text-muted-foreground size-4" />
        <h2 className="text-muted-foreground font-mono text-xs tracking-wide uppercase">
          Terminal
        </h2>
        <span className="text-muted-foreground ml-auto font-mono text-[10px]">
          bounded run · not a pty
        </span>
      </div>

      <div
        ref={scrollRef}
        className="bg-surface-tertiary flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto rounded-md p-3 font-mono text-xs"
      >
        {entries.length === 0 ? (
          <p className="text-muted-foreground">Run a command to poke the environment.</p>
        ) : (
          entries.map((e, i) => <TerminalRow key={i} entry={e} />)
        )}
      </div>

      <div className="bg-surface-tertiary flex shrink-0 items-center gap-2 rounded-md px-3 py-2 font-mono text-xs">
        <span className="text-pax select-none">$</span>
        <input
          value={cmd}
          autoComplete="off"
          autoCorrect="off"
          spellCheck={false}
          placeholder={busy ? 'running…' : 'type a command…'}
          disabled={busy}
          onChange={(ev) => setCmd(ev.target.value)}
          onKeyDown={(ev) => {
            if (ev.key === 'Enter') {
              ev.preventDefault()
              void run()
            }
          }}
          className="placeholder:text-muted-foreground text-foreground flex-1 bg-transparent outline-none disabled:opacity-60"
        />
        {busy ? <CodyLoader variant="ring" size={14} /> : null}
      </div>
    </section>
  )
}

function TerminalRow({ entry }: { entry: TerminalEntry }) {
  const { cmd, result, error, durationMs, running } = entry
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        <span className="text-pax select-none">$</span>
        <span className="break-all whitespace-pre-wrap">{cmd}</span>
      </div>

      {running ? (
        <div className="text-muted-foreground pl-4">
          <CodyLoader variant="dots" />
        </div>
      ) : null}

      {result && result.output ? (
        <pre className="text-muted-foreground overflow-x-auto pl-4 break-words whitespace-pre-wrap">
          {result.output}
        </pre>
      ) : null}

      {error ? (
        <div className="text-destructive flex items-center gap-2 pl-4">
          <IconAlertCircle className="size-3.5 shrink-0" />
          <span className="break-words whitespace-pre-wrap">{error}</span>
        </div>
      ) : null}

      {!running ? (
        <div className="text-muted-foreground flex items-center gap-2 pl-4 text-[10px]">
          {error ? (
            <span className="text-destructive">error</span>
          ) : (
            <span className={result && result.exit === 0 ? 'text-pax' : 'text-destructive'}>
              exit {result?.exit ?? '—'}
            </span>
          )}
          {result?.timed_out ? <span className="text-destructive">timed out</span> : null}
          {durationMs != null ? <span>{formatDuration(durationMs)}</span> : null}
        </div>
      ) : null}
    </div>
  )
}

// ── Spec viewer ─────────────────────────────────────────────────────────────

interface SpecTaskItem {
  checked: boolean
  text: string
  /** Leading whitespace depth (in spaces) for nested subtasks. */
  indent: number
}

/** Parse markdown task-list checkboxes (`- [ ]` / `- [x]`) into a checklist. */
function parseTasks(md: string): SpecTaskItem[] {
  const items: SpecTaskItem[] = []
  for (const raw of md.split('\n')) {
    const m = raw.match(/^(\s*)[-*]\s+\[([ xX])\]\s+(.*)$/)
    if (!m) continue
    items.push({
      indent: m[1].replace(/\t/g, '  ').length,
      checked: m[2].toLowerCase() === 'x',
      text: m[3].trim(),
    })
  }
  return items
}

interface SpecState {
  requirements: string | null
  tasks: string | null
  loading: boolean
  error: string | null
}

/** Fetch a spec file; a 404 resolves to null (file simply not authored yet). */
async function fetchSpecFile(path: string, projectID?: string, signal?: AbortSignal) {
  try {
    const file = await getFile(path, projectID, signal)
    return file.content
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null
    throw err
  }
}

export function CodySpecViewer({ projectID }: { projectID?: string }) {
  const [state, setState] = useState<SpecState>({
    requirements: null,
    tasks: null,
    loading: true,
    error: null,
  })

  useEffect(() => {
    const ctrl = new AbortController()
    setState({ requirements: null, tasks: null, loading: true, error: null })
    Promise.all([
      fetchSpecFile('.cody/spec/requirements.md', projectID, ctrl.signal),
      fetchSpecFile('.cody/spec/tasks.md', projectID, ctrl.signal),
    ])
      .then(([requirements, tasks]) => {
        setState({ requirements, tasks, loading: false, error: null })
      })
      .catch((err) => {
        if (ctrl.signal.aborted) return
        const message = err instanceof Error ? err.message : 'Failed to load the spec'
        setState({ requirements: null, tasks: null, loading: false, error: message })
      })
    return () => ctrl.abort()
  }, [projectID])

  if (state.loading) {
    return (
      <section className="bg-surface-secondary flex h-full items-center justify-center rounded-lg p-4">
        <CodyLoader variant="ring" label="Loading spec…" />
      </section>
    )
  }

  if (state.error) {
    return (
      <section className="bg-surface-secondary flex h-full flex-col items-center justify-center gap-2 rounded-lg p-4">
        <IconAlertCircle className="text-destructive size-5" />
        <p className="text-muted-foreground text-center text-sm">{state.error}</p>
      </section>
    )
  }

  const hasSpec = state.requirements != null || state.tasks != null
  if (!hasSpec) {
    return (
      <section className="bg-surface-secondary flex h-full flex-col items-center justify-center gap-2 rounded-lg p-6 text-center">
        <IconFileText className="text-muted-foreground size-6" />
        <p className="text-sm font-medium">No durable spec yet</p>
        <p className="text-muted-foreground max-w-xs text-xs">
          This project has no .cody/spec. Architect runs author requirements.md and tasks.md;
          Prototype and Engineer projects don&apos;t.
        </p>
      </section>
    )
  }

  const tasks = state.tasks != null ? parseTasks(state.tasks) : []
  const done = tasks.filter((t) => t.checked).length

  return (
    <section className="bg-surface-secondary flex h-full min-h-0 flex-col gap-4 overflow-y-auto rounded-lg p-4">
      {state.tasks != null ? (
        <div className="flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <h2 className="text-muted-foreground font-mono text-xs tracking-wide uppercase">
              Tasks
            </h2>
            {tasks.length > 0 ? (
              <span className="text-muted-foreground ml-auto font-mono text-[10px]">
                {done}/{tasks.length}
              </span>
            ) : null}
          </div>
          {tasks.length > 0 ? (
            <ul className="flex flex-col gap-1">
              {tasks.map((t, i) => (
                <li
                  key={i}
                  className="bg-surface-tertiary flex items-start gap-2 rounded-md px-3 py-1.5"
                  style={{ marginLeft: t.indent >= 2 ? Math.min(t.indent, 8) * 8 : 0 }}
                >
                  <SpecCheckbox checked={t.checked} />
                  <span
                    className={t.checked ? 'text-muted-foreground text-sm line-through' : 'text-sm'}
                  >
                    {t.text}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-muted-foreground text-xs">No task checkboxes in tasks.md.</p>
          )}
        </div>
      ) : null}

      {state.requirements != null ? (
        <div className="flex flex-col gap-2">
          <h2 className="text-muted-foreground font-mono text-xs tracking-wide uppercase">
            Requirements
          </h2>
          <pre className="text-foreground overflow-x-auto text-xs break-words whitespace-pre-wrap">
            {state.requirements}
          </pre>
        </div>
      ) : null}
    </section>
  )
}

function SpecCheckbox({ checked }: { checked: boolean }) {
  if (checked) {
    return (
      <span className="bg-pax mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-sm">
        <IconCheck className="text-background size-3" />
      </span>
    )
  }
  return <span className="bg-surface-hover mt-0.5 size-4 shrink-0 rounded-sm" />
}
