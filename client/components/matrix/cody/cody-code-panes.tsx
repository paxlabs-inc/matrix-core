'use client'

/**
 * Cody code panes (task 6.3) — the read surfaces over a project workspace:
 *   - CodyFileTree  : the getTree file browser (honest truncation; click → open).
 *   - CodyFileViewer: a read-only file view via restyled Shiki (no borders).
 *   - CodyDiffPane  : a PASSIVE audit of getDiff — staged+unstaged hunks plus
 *                     untracked paths. No approve/reject; task-level blocking
 *                     review is a non-goal here.
 *
 * These are presentational panes taking a projectID. Each fetches its own data
 * via the lib/api/cody.ts wrappers (useEffect + AbortController), matching the
 * rest of the client. Layers separate by background tone — never border strokes.
 */
import { useEffect, useMemo, useState } from 'react'

import type { BundledLanguage } from 'shiki'

import { CodeBlockContent } from '@/components/ai-elements/code-block'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import {
  IconAlertCircle,
  IconChevronDown,
  IconChevronRight,
  IconFile,
  IconFolder,
  IconFolderOpen,
  IconRefresh,
} from '@/components/matrix/cody/icons'
import { Button } from '@/components/ui/button'
import {
  getDiff,
  getFile,
  getTree,
  type TreeEntry,
  type WorkspaceDiff,
  type WorkspaceFile,
  type WorkspaceTree,
} from '@/lib/api/cody'

// ── Shared fetch primitive ────────────────────────────────────────────────────

interface Resource<T> {
  data: T | null
  error: string | null
  loading: boolean
  reload: () => void
}

/** Fetch a cody resource with an AbortController, reload, and honest error. */
function useResource<T>(
  load: (signal: AbortSignal) => Promise<T>,
  deps: React.DependencyList,
): Resource<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    let live = true
    setLoading(true)
    setError(null)
    load(controller.signal)
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .then((result) => {
        if (!live) return
        setData(result)
        setLoading(false)
      })
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .catch((err: unknown) => {
        if (!live || controller.signal.aborted) return
        setData(null)
        setError(err instanceof Error ? err.message : 'Failed to load')
        setLoading(false)
      })
    return () => {
      live = false
      controller.abort()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  return { data, error, loading, reload: () => setNonce((n) => n + 1) }
}

// ── Shared pane chrome ────────────────────────────────────────────────────────

function PaneLabel({ children }: { children: React.ReactNode }) {
  return (
    <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
      {children}
    </span>
  )
}

function PaneError({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="flex flex-col items-center gap-3 p-6 text-center">
      <IconAlertCircle className="text-destructive size-5" />
      <p className="text-muted-foreground text-sm">{message}</p>
      <Button size="sm" variant="ghost" onClick={onRetry}>
        <IconRefresh className="size-3.5" />
        <span>Retry</span>
      </Button>
    </div>
  )
}

function PaneEmpty({ children }: { children: React.ReactNode }) {
  return <p className="text-muted-foreground p-6 text-center text-sm">{children}</p>
}

function TruncationNote({ children }: { children: React.ReactNode }) {
  return (
    <div className="bg-surface-tertiary text-muted-foreground flex items-center gap-2 rounded-md px-3 py-2 font-mono text-[11px]">
      <IconAlertCircle className="size-3.5 shrink-0" />
      <span>{children}</span>
    </div>
  )
}

// ── File tree ─────────────────────────────────────────────────────────────────

interface TreeNode {
  name: string
  path: string
  dir: boolean
  children: TreeNode[]
}

/** Fold the flat, path-keyed entries into a nested tree, creating parents. */
function buildTree(entries: TreeEntry[]): TreeNode {
  const root: TreeNode = { name: '', path: '', dir: true, children: [] }
  const byPath = new Map<string, TreeNode>([['', root]])

  const ensure = (path: string, dir: boolean): TreeNode => {
    const existing = byPath.get(path)
    if (existing) {
      if (dir) existing.dir = true
      return existing
    }
    const parts = path.split('/')
    const name = parts[parts.length - 1]
    const parent = ensure(parts.slice(0, -1).join('/'), true)
    const node: TreeNode = { name, path, dir, children: [] }
    parent.children.push(node)
    byPath.set(path, node)
    return node
  }

  for (const entry of entries) {
    const path = entry.path.replace(/^\/+/, '').replace(/\/+$/, '')
    if (path) ensure(path, entry.dir)
  }
  return root
}

/** Directories first, then case-insensitive by name. */
function sortNodes(nodes: TreeNode[]): TreeNode[] {
  return [...nodes].sort((a, b) => {
    if (a.dir !== b.dir) return a.dir ? -1 : 1
    return a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
  })
}

export function CodyFileTree({
  projectID,
  onOpen,
}: {
  projectID?: string
  onOpen: (path: string) => void
}) {
  const res = useResource<WorkspaceTree>((signal) => getTree(projectID, signal), [projectID])
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  const tree = useMemo(() => (res.data ? buildTree(res.data.entries) : null), [res.data])

  // Seed with the top-level directories open so the tree isn't a wall of folders.
  useEffect(() => {
    if (!tree) return
    setExpanded(new Set(tree.children.filter((n) => n.dir).map((n) => n.path)))
  }, [tree])

  const toggle = (path: string) =>
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(path)) next.delete(path)
      else next.add(path)
      return next
    })

  return (
    <section className="bg-surface-secondary flex min-h-0 flex-col gap-3 rounded-lg p-3">
      <div className="flex items-center gap-2">
        <PaneLabel>Files</PaneLabel>
        {res.data ? (
          <span className="text-muted-foreground ml-auto font-mono text-[10px]">
            {res.data.entries.length}
          </span>
        ) : null}
      </div>

      {res.loading ? (
        <CodyLoader variant="ring" size={28} className="m-auto py-6" />
      ) : res.error ? (
        <PaneError message={res.error} onRetry={res.reload} />
      ) : !tree || tree.children.length === 0 ? (
        <PaneEmpty>No files in this workspace yet.</PaneEmpty>
      ) : (
        <div className="flex min-h-0 flex-col gap-2 overflow-y-auto">
          <div className="flex flex-col">
            {sortNodes(tree.children).map((node) => (
              <TreeRow
                key={node.path}
                node={node}
                depth={0}
                expanded={expanded}
                onToggle={toggle}
                onOpen={onOpen}
              />
            ))}
          </div>
          {res.data?.truncated ? (
            <TruncationNote>Tree truncated — not every file is listed.</TruncationNote>
          ) : null}
        </div>
      )}
    </section>
  )
}

function TreeRow({
  node,
  depth,
  expanded,
  onToggle,
  onOpen,
}: {
  node: TreeNode
  depth: number
  expanded: Set<string>
  onToggle: (path: string) => void
  onOpen: (path: string) => void
}) {
  const isOpen = expanded.has(node.path)
  const pad = { paddingLeft: 6 + depth * 14 }

  if (!node.dir) {
    return (
      <button
        type="button"
        onClick={() => onOpen(node.path)}
        style={pad}
        className="hover:bg-surface-hover flex w-full items-center gap-2 rounded-md py-1 pr-2 text-left text-sm"
      >
        <IconFile className="text-muted-foreground size-4 shrink-0" />
        <span className="truncate font-mono text-xs">{node.name}</span>
      </button>
    )
  }

  return (
    <>
      <button
        type="button"
        onClick={() => onToggle(node.path)}
        style={pad}
        className="hover:bg-surface-hover flex w-full items-center gap-1.5 rounded-md py-1 pr-2 text-left text-sm"
      >
        {isOpen ? (
          <IconChevronDown className="text-muted-foreground size-3.5 shrink-0" />
        ) : (
          <IconChevronRight className="text-muted-foreground size-3.5 shrink-0" />
        )}
        {isOpen ? (
          <IconFolderOpen className="text-muted-foreground size-4 shrink-0" />
        ) : (
          <IconFolder className="text-muted-foreground size-4 shrink-0" />
        )}
        <span className="truncate font-mono text-xs">{node.name}</span>
      </button>
      {isOpen
        ? sortNodes(node.children).map((child) => (
            <TreeRow
              key={child.path}
              node={child}
              depth={depth + 1}
              expanded={expanded}
              onToggle={onToggle}
              onOpen={onOpen}
            />
          ))
        : null}
    </>
  )
}

// ── File viewer ───────────────────────────────────────────────────────────────

const LANG_BY_EXT: Record<string, string> = {
  ts: 'typescript',
  tsx: 'tsx',
  js: 'javascript',
  jsx: 'jsx',
  mjs: 'javascript',
  cjs: 'javascript',
  json: 'json',
  jsonc: 'jsonc',
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
  zsh: 'bash',
  sql: 'sql',
  html: 'html',
  css: 'css',
  scss: 'scss',
  less: 'less',
  vue: 'vue',
  svelte: 'svelte',
  md: 'markdown',
  mdx: 'mdx',
  yaml: 'yaml',
  yml: 'yaml',
  toml: 'toml',
  xml: 'xml',
  dockerfile: 'docker',
  makefile: 'makefile',
  proto: 'proto',
  graphql: 'graphql',
  gql: 'graphql',
  kvx: 'ini',
  env: 'ini',
  ini: 'ini',
}

/** Map a workspace path to a Shiki language id; unknown falls back to plain text. */
function languageFor(path: string): BundledLanguage {
  const base = path.split('/').pop() ?? path
  const lower = base.toLowerCase()
  const byName = LANG_BY_EXT[lower]
  if (byName) return byName as BundledLanguage
  const ext = lower.includes('.') ? lower.slice(lower.lastIndexOf('.') + 1) : ''
  return (LANG_BY_EXT[ext] ?? 'text') as BundledLanguage
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

export function CodyFileViewer({ projectID, path }: { projectID?: string; path: string }) {
  const res = useResource<WorkspaceFile>(
    (signal) => getFile(path, projectID, signal),
    [projectID, path],
  )
  const language = useMemo(() => languageFor(path), [path])

  return (
    <section className="bg-surface-secondary flex min-h-0 flex-col gap-3 rounded-lg p-3">
      <div className="flex items-center gap-2">
        <span className="truncate font-mono text-xs" title={path}>
          {path}
        </span>
        {res.data ? (
          <span className="text-muted-foreground ml-auto shrink-0 font-mono text-[10px]">
            {formatBytes(res.data.size)}
          </span>
        ) : null}
      </div>

      {res.loading ? (
        <CodyLoader variant="ring" size={28} className="m-auto py-6" />
      ) : res.error ? (
        <PaneError message={res.error} onRetry={res.reload} />
      ) : !res.data ? (
        <PaneEmpty>Select a file to view.</PaneEmpty>
      ) : res.data.content === '' ? (
        <PaneEmpty>This file is empty.</PaneEmpty>
      ) : (
        <div className="flex min-h-0 flex-col gap-2 overflow-hidden">
          <div className="bg-surface-tertiary min-h-0 overflow-auto rounded-md">
            <CodeBlockContent code={res.data.content} language={language} showLineNumbers />
          </div>
          {res.data.truncated ? (
            <TruncationNote>File truncated — showing the first portion only.</TruncationNote>
          ) : null}
        </div>
      )}
    </section>
  )
}

// ── Diff pane ─────────────────────────────────────────────────────────────────

type DiffLineKind = 'add' | 'del' | 'ctx' | 'meta'

interface DiffLine {
  kind: DiffLineKind
  text: string
}

interface DiffHunk {
  header: string
  lines: DiffLine[]
}

interface DiffFile {
  path: string
  hunks: DiffHunk[]
}

/** Parse a unified git diff into per-file hunks for a passive, hunk-level read. */
function parseDiff(diff: string): DiffFile[] {
  const files: DiffFile[] = []
  let file: DiffFile | null = null
  let hunk: DiffHunk | null = null

  for (const line of diff.split('\n')) {
    if (line.startsWith('diff --git')) {
      const m = line.match(/ b\/(.+)$/)
      file = { path: m ? m[1] : line.replace('diff --git ', ''), hunks: [] }
      files.push(file)
      hunk = null
      continue
    }
    if (line.startsWith('+++ ')) {
      const target = line.slice(4).replace(/^b\//, '')
      if (file && target && target !== '/dev/null') file.path = target
      continue
    }
    if (
      line.startsWith('--- ') ||
      line.startsWith('index ') ||
      line.startsWith('new file') ||
      line.startsWith('deleted file') ||
      line.startsWith('similarity ') ||
      line.startsWith('rename ') ||
      line.startsWith('old mode') ||
      line.startsWith('new mode')
    ) {
      continue
    }
    if (line.startsWith('@@')) {
      if (!file) {
        file = { path: 'changes', hunks: [] }
        files.push(file)
      }
      hunk = { header: line, lines: [] }
      file.hunks.push(hunk)
      continue
    }
    if (!hunk) continue
    if (line.startsWith('+')) hunk.lines.push({ kind: 'add', text: line.slice(1) })
    else if (line.startsWith('-')) hunk.lines.push({ kind: 'del', text: line.slice(1) })
    else if (line.startsWith('\\')) hunk.lines.push({ kind: 'meta', text: line })
    else hunk.lines.push({ kind: 'ctx', text: line.startsWith(' ') ? line.slice(1) : line })
  }
  return files
}

export function CodyDiffPane({ projectID }: { projectID?: string }) {
  const res = useResource<WorkspaceDiff>((signal) => getDiff(projectID, signal), [projectID])
  const files = useMemo(() => (res.data ? parseDiff(res.data.diff) : []), [res.data])

  const untracked = res.data?.untracked ?? []
  const nothing = res.data && res.data.git && files.length === 0 && untracked.length === 0

  return (
    <section className="bg-surface-secondary flex min-h-0 flex-col gap-3 rounded-lg p-3">
      <div className="flex items-center gap-2">
        <PaneLabel>Working changes</PaneLabel>
        {res.data ? (
          <span className="text-muted-foreground ml-auto font-mono text-[10px]">
            {files.length + untracked.length} changed
          </span>
        ) : null}
      </div>

      {res.loading ? (
        <CodyLoader variant="ring" size={28} className="m-auto py-6" />
      ) : res.error ? (
        <PaneError message={res.error} onRetry={res.reload} />
      ) : res.data && !res.data.git ? (
        <PaneEmpty>This workspace is not a git repository.</PaneEmpty>
      ) : nothing ? (
        <PaneEmpty>No uncommitted changes.</PaneEmpty>
      ) : (
        <div className="flex min-h-0 flex-col gap-3 overflow-y-auto">
          {files.map((file, i) => (
            <DiffFileBlock key={`${file.path}:${i}`} file={file} />
          ))}
          {untracked.length > 0 ? <UntrackedBlock paths={untracked} /> : null}
        </div>
      )}
    </section>
  )
}

function diffLineClass(kind: DiffLineKind): string {
  if (kind === 'add') return 'text-pax'
  if (kind === 'del') return 'text-destructive'
  if (kind === 'meta') return 'text-muted-foreground'
  return 'text-foreground/80'
}

function diffLinePrefix(kind: DiffLineKind): string {
  if (kind === 'add') return '+'
  if (kind === 'del') return '-'
  return ' '
}

function DiffFileBlock({ file }: { file: DiffFile }) {
  return (
    <div className="bg-surface-tertiary flex flex-col gap-2 rounded-md p-3">
      <span className="truncate font-mono text-xs" title={file.path}>
        {file.path}
      </span>
      <div className="flex flex-col gap-2 overflow-x-auto">
        {file.hunks.map((h, hi) => (
          <div key={hi} className="flex flex-col">
            <span className="text-muted-foreground font-mono text-[11px] whitespace-pre">
              {h.header}
            </span>
            {h.lines.map((ln, li) => (
              <span
                key={li}
                className={`font-mono text-xs whitespace-pre ${diffLineClass(ln.kind)}`}
              >
                {ln.kind === 'meta' ? ln.text : `${diffLinePrefix(ln.kind)}${ln.text}`}
              </span>
            ))}
          </div>
        ))}
      </div>
    </div>
  )
}

function UntrackedBlock({ paths }: { paths: string[] }) {
  return (
    <div className="bg-surface-tertiary flex flex-col gap-2 rounded-md p-3">
      <PaneLabel>Untracked</PaneLabel>
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
