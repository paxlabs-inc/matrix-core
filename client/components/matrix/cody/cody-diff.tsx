'use client'

/**
 * CodyDiffView — the diff review pane at the Claude Code bar (req 12.3).
 *
 * Renders the workspace's working changes (GET /workspace/diff — a unified git
 * diff plus untracked paths) through @git-diff-view/react with the Shiki
 * highlighter, so diffs carry the same VSCode-grade syntax fidelity as the
 * code viewer. Split/unified toggle, per-file collapse, add/del line counts.
 * Layers separate by background tone — no border strokes.
 */
import { useEffect, useMemo, useState } from 'react'

import { DiffView, DiffModeEnum } from '@git-diff-view/react'
import { getDiffViewHighlighter } from '@git-diff-view/shiki'
import type { DiffHighlighter } from '@git-diff-view/shiki'
import '@git-diff-view/react/styles/diff-view.css'

import { Button } from '@/components/ui/button'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import {
  IconAlertCircle,
  IconChevronDown,
  IconChevronRight,
  IconRefresh,
} from '@/components/matrix/cody/icons'
import { getDiff, type WorkspaceDiff } from '@/lib/api/cody'
import { cn } from '@/lib/utils'

// ── Language inference ────────────────────────────────────────────────────────

const LANG_BY_EXT: Record<string, string> = {
  ts: 'typescript',
  tsx: 'tsx',
  js: 'javascript',
  jsx: 'jsx',
  mjs: 'javascript',
  cjs: 'javascript',
  json: 'json',
  go: 'go',
  py: 'python',
  rb: 'ruby',
  rs: 'rust',
  java: 'java',
  kt: 'kotlin',
  c: 'c',
  h: 'c',
  cpp: 'cpp',
  cc: 'cpp',
  hpp: 'cpp',
  cs: 'csharp',
  php: 'php',
  swift: 'swift',
  sh: 'bash',
  bash: 'bash',
  sql: 'sql',
  html: 'html',
  css: 'css',
  scss: 'scss',
  vue: 'vue',
  svelte: 'svelte',
  md: 'markdown',
  mdx: 'mdx',
  yaml: 'yaml',
  yml: 'yaml',
  toml: 'toml',
  xml: 'xml',
}

function langFor(path: string): string {
  const lower = (path.split('/').pop() ?? path).toLowerCase()
  const ext = lower.includes('.') ? lower.slice(lower.lastIndexOf('.') + 1) : ''
  return LANG_BY_EXT[ext] ?? 'plaintext'
}

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

// ── Shiki highlighter (module-level, shared across renders) ──────────────────

let highlighterPromise: Promise<DiffHighlighter> | null = null

function loadHighlighter(): Promise<DiffHighlighter> {
  highlighterPromise ??= getDiffViewHighlighter()
  return highlighterPromise
}

// ── The pane ──────────────────────────────────────────────────────────────────

export function CodyDiffView({ projectID }: { projectID?: string }) {
  const [data, setData] = useState<WorkspaceDiff | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)
  const [split, setSplit] = useState(false)
  const [highlighter, setHighlighter] = useState<DiffHighlighter | null>(null)

  useEffect(() => {
    let live = true
    loadHighlighter()
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .then((h) => {
        if (live) setHighlighter(h)
      })
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .catch(() => {
        /* highlighting is progressive enhancement — the diff renders without it */
      })
    return () => {
      live = false
    }
  }, [])

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
            <FileDiffBlock
              key={file.path}
              file={file}
              split={split}
              highlighter={highlighter}
            />
          ))}
          {untracked.length > 0 ? <UntrackedBlock paths={untracked} /> : null}
        </div>
      )}
    </section>
  )
}

function FileDiffBlock({
  file,
  split,
  highlighter,
}: {
  file: FileDiff
  split: boolean
  highlighter: DiffHighlighter | null
}) {
  const [open, setOpen] = useState(true)
  const lang = langFor(file.path)
  const diffData = useMemo(
    () => ({
      oldFile: { fileName: file.path, fileLang: lang },
      newFile: { fileName: file.path, fileLang: lang },
      hunks: [file.hunks],
    }),
    [file, lang],
  )
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
        <div className="cody-diff-frame min-w-0 overflow-x-auto">
          <DiffView
            data={diffData}
            diffViewMode={split ? DiffModeEnum.Split : DiffModeEnum.Unified}
            diffViewTheme="dark"
            diffViewHighlight={highlighter !== null}
            registerHighlighter={highlighter ?? undefined}
            diffViewFontSize={12}
            diffViewWrap={false}
          />
        </div>
      ) : null}
    </div>
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
