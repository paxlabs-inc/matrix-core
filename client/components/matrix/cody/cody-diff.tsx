'use client'

/**
 * CodyDiffView — the diff review pane at the Claude Code bar (req 12.3).
 *
 * Renders the workspace's working changes (GET /workspace/diff — a unified git
 * diff plus untracked paths) as a line-numbered native review surface.
 * Split/unified toggle, per-file collapse, add/del line counts. Layers
 * separate by background tone — no border strokes.
 */
import { useEffect, useMemo, useState } from 'react'

import { Button } from '@/components/ui/button'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import {
  IconAlertCircle,
  IconChevronDown,
  IconChevronRight,
  IconRefresh,
} from '@/components/matrix/cody/icons'
import { getDiff, type WorkspaceDiff } from '@/lib/api/workspace'
import { cn } from '@/lib/utils'

// ── Unified-diff splitting ────────────────────────────────────────────────────

export interface FileDiff {
  path: string
  /** The raw hunk text (from the first @@ onward) for this file. */
  hunks: string
  adds: number
  dels: number
}

/** Split a repo-wide unified git diff into per-file hunk blocks. */
export function splitUnifiedDiff(diff: string): FileDiff[] {
  const files: FileDiff[] = []
  let path = ''
  let lines: string[] = []
  let inHunks = false
  let adds = 0
  let dels = 0

  const flush = () => {
    if (path && lines.length > 0) files.push({ path, hunks: lines.join('\n'), adds, dels })
    lines = []
    inHunks = false
    adds = 0
    dels = 0
  }

  for (const line of diff.split('\n')) {
    if (line.startsWith('diff --git')) {
      flush()
      const m = line.match(/ b\/(.+)$/)
      path = m ? m[1] : line.replace('diff --git ', '')
      continue
    }
    if (line.startsWith('+++ ')) {
      const target = line.slice(4).replace(/^b\//, '')
      if (target && target !== '/dev/null') path = target
      continue
    }
    if (line.startsWith('@@')) {
      inHunks = true
      lines.push(line)
      continue
    }
    if (!inHunks) continue
    lines.push(line)
    if (line.startsWith('+')) adds++
    else if (line.startsWith('-')) dels++
  }
  flush()
  return files
}

type DiffLineKind = 'context' | 'add' | 'delete' | 'hunk' | 'meta'

export interface ParsedDiffLine {
  kind: DiffLineKind
  text: string
  oldLine?: number
  newLine?: number
}

export function parseDiffHunks(hunks: string): ParsedDiffLine[] {
  const rows: ParsedDiffLine[] = []
  let oldLine = 0
  let newLine = 0
  for (const line of hunks.split('\n')) {
    if (line.startsWith('@@')) {
      const range = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/)
      if (range) {
        oldLine = Number(range[1])
        newLine = Number(range[2])
      }
      rows.push({ kind: 'hunk', text: line })
      continue
    }
    if (line.startsWith('+')) {
      rows.push({ kind: 'add', text: line.slice(1), newLine })
      newLine++
      continue
    }
    if (line.startsWith('-')) {
      rows.push({ kind: 'delete', text: line.slice(1), oldLine })
      oldLine++
      continue
    }
    if (line.startsWith(' ')) {
      rows.push({ kind: 'context', text: line.slice(1), oldLine, newLine })
      oldLine++
      newLine++
      continue
    }
    if (line) rows.push({ kind: 'meta', text: line })
  }
  return rows
}

// ── The pane ──────────────────────────────────────────────────────────────────

export function CodyDiffView({ projectID }: { projectID?: string }) {
  const [data, setData] = useState<WorkspaceDiff | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)
  const [split, setSplit] = useState(false)

  useEffect(() => {
    const ctrl = new AbortController()
    let live = true
    setLoading(true)
    setError(null)
    getDiff(projectID, ctrl.signal)
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .then((d) => {
        if (!live) return
        setData(d)
        setLoading(false)
      })
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .catch((err: unknown) => {
        if (!live || ctrl.signal.aborted) return
        setError(err instanceof Error ? err.message : 'Failed to load the diff')
        setLoading(false)
      })
    return () => {
      live = false
      ctrl.abort()
    }
  }, [projectID, nonce])

  const files = useMemo(() => (data ? splitUnifiedDiff(data.diff) : []), [data])
  const untracked = data?.untracked ?? []
  const nothing = data && data.git && files.length === 0 && untracked.length === 0

  return (
    <section className="bg-surface-secondary flex h-full min-h-0 flex-col gap-3 rounded-lg p-3">
      <div className="flex shrink-0 items-center gap-2">
        <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
          Working changes
        </span>
        {data ? (
          <span className="text-muted-foreground font-mono text-[10px]">
            {files.length + untracked.length} changed
          </span>
        ) : null}
        <div className="bg-surface-tertiary ml-auto flex gap-0.5 rounded-md p-0.5">
          {(['unified', 'split'] as const).map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => setSplit(m === 'split')}
              className={cn(
                'rounded px-2 py-0.5 font-mono text-[10px] uppercase transition-colors',
                (m === 'split') === split
                  ? 'bg-surface-hover text-foreground'
                  : 'text-muted-foreground',
              )}
            >
              {m}
            </button>
          ))}
        </div>
      </div>

      {loading ? (
        <CodyLoader variant="ring" size={28} className="m-auto py-6" />
      ) : error ? (
        <div className="flex flex-col items-center gap-3 p-6 text-center">
          <IconAlertCircle className="text-destructive size-5" />
          <p className="text-muted-foreground text-sm">{error}</p>
          <Button size="sm" variant="ghost" onClick={() => setNonce((n) => n + 1)}>
            <IconRefresh className="size-3.5" />
            <span>Retry</span>
          </Button>
        </div>
      ) : data && !data.git ? (
        <p className="text-muted-foreground p-6 text-center text-sm">
          This workspace is not a git repository.
        </p>
      ) : nothing ? (
        <p className="text-muted-foreground p-6 text-center text-sm">No uncommitted changes.</p>
      ) : (
        <div className="flex min-h-0 flex-col gap-2 overflow-y-auto">
          {files.map((file) => (
            <FileDiffBlock key={file.path} file={file} split={split} />
          ))}
          {untracked.length > 0 ? <UntrackedBlock paths={untracked} /> : null}
        </div>
      )}
    </section>
  )
}

function FileDiffBlock({ file, split }: { file: FileDiff; split: boolean }) {
  const [open, setOpen] = useState(true)
  const rows = useMemo(() => parseDiffHunks(file.hunks), [file.hunks])
  return (
    <div className="bg-surface-tertiary flex flex-col overflow-hidden rounded-md">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="hover:bg-surface-hover flex w-full items-center gap-2 px-3 py-2 text-left"
      >
        {open ? (
          <IconChevronDown className="text-muted-foreground size-3.5 shrink-0" />
        ) : (
          <IconChevronRight className="text-muted-foreground size-3.5 shrink-0" />
        )}
        <span className="truncate font-mono text-xs" title={file.path}>
          {file.path}
        </span>
        <span className="ml-auto flex shrink-0 items-center gap-2 font-mono text-[10px]">
          <span className="text-pax">+{file.adds}</span>
          <span className="text-destructive">-{file.dels}</span>
        </span>
      </button>
      {open ? (
        <div className="min-w-0 overflow-x-auto" aria-label={`Diff for ${file.path}`}>
          {split ? <SplitDiff rows={rows} /> : <UnifiedDiff rows={rows} />}
        </div>
      ) : null}
    </div>
  )
}

function rowTone(kind: DiffLineKind): string {
  if (kind === 'add') return 'bg-emerald-500/10'
  if (kind === 'delete') return 'bg-red-500/10'
  if (kind === 'hunk') return 'bg-surface-hover text-muted-foreground'
  if (kind === 'meta') return 'bg-surface-secondary text-muted-foreground'
  return ''
}

function LineNumber({ value }: { value?: number }) {
  return (
    <span className="text-muted-foreground w-10 shrink-0 px-2 text-right select-none">
      {value ?? ''}
    </span>
  )
}

function UnifiedDiff({ rows }: { rows: ParsedDiffLine[] }) {
  return (
    <pre className="min-w-max py-1 font-mono text-xs leading-5">
      {rows.map((row, index) =>
        row.kind === 'hunk' || row.kind === 'meta' ? (
          <div key={index} className={cn('flex min-h-5 px-2', rowTone(row.kind))}>
            {row.text}
          </div>
        ) : (
          <div key={index} className={cn('flex min-h-5', rowTone(row.kind))}>
            <LineNumber value={row.oldLine} />
            <LineNumber value={row.newLine} />
            <span className="text-muted-foreground w-5 shrink-0 text-center select-none">
              {row.kind === 'add' ? '+' : row.kind === 'delete' ? '-' : ' '}
            </span>
            <span className="pr-4 whitespace-pre">{row.text}</span>
          </div>
        ),
      )}
    </pre>
  )
}

interface SplitRow {
  left?: ParsedDiffLine
  right?: ParsedDiffLine
  marker?: ParsedDiffLine
}

function splitDiffRows(rows: ParsedDiffLine[]): SplitRow[] {
  const output: SplitRow[] = []
  let deletes: ParsedDiffLine[] = []
  let adds: ParsedDiffLine[] = []
  const flushChanges = () => {
    const length = Math.max(deletes.length, adds.length)
    for (let index = 0; index < length; index++) {
      output.push({ left: deletes[index], right: adds[index] })
    }
    deletes = []
    adds = []
  }
  for (const row of rows) {
    if (row.kind === 'delete') {
      deletes.push(row)
    } else if (row.kind === 'add') {
      adds.push(row)
    } else {
      flushChanges()
      if (row.kind === 'context') output.push({ left: row, right: row })
      else output.push({ marker: row })
    }
  }
  flushChanges()
  return output
}

function SplitCell({ row, side }: { row?: ParsedDiffLine; side: 'old' | 'new' }) {
  return (
    <div className={cn('flex min-h-5 min-w-0', row ? rowTone(row.kind) : 'bg-surface-secondary')}>
      <LineNumber value={row?.[side === 'old' ? 'oldLine' : 'newLine']} />
      <span className="text-muted-foreground w-5 shrink-0 text-center select-none">
        {row?.kind === 'add' ? '+' : row?.kind === 'delete' ? '-' : ' '}
      </span>
      <span className="min-w-0 pr-4 whitespace-pre">{row?.text ?? ''}</span>
    </div>
  )
}

function SplitDiff({ rows }: { rows: ParsedDiffLine[] }) {
  const splitRows = useMemo(() => splitDiffRows(rows), [rows])
  return (
    <pre className="grid min-w-[48rem] grid-cols-2 py-1 font-mono text-xs leading-5">
      {splitRows.map((row, index) =>
        row.marker ? (
          <div key={index} className={cn('col-span-2 min-h-5 px-2', rowTone(row.marker.kind))}>
            {row.marker.text}
          </div>
        ) : (
          <div key={index} className="col-span-2 grid grid-cols-2">
            <SplitCell row={row.left} side="old" />
            <SplitCell row={row.right} side="new" />
          </div>
        ),
      )}
    </pre>
  )
}

function UntrackedBlock({ paths }: { paths: string[] }) {
  return (
    <div className="bg-surface-tertiary flex flex-col gap-2 rounded-md p-3">
      <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
        Untracked
      </span>
      <ul className="flex flex-col gap-1">
        {paths.map((p) => (
          <li key={p} className="flex items-center gap-2 font-mono text-xs">
            <span className="text-pax w-4 shrink-0 text-center">+</span>
            <span className="truncate">{p}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
