'use client'

/**
 * CodyTree — the workspace file tree at the Claude Code bar (req 12.3).
 *
 * A virtualized, keyboard-navigable tree over GET /workspace/tree via
 * react-arborist (the VSCode-sidebar-grade engine: aria, filtering, selection
 * sync, virtualized rows), rendered in Matrix chrome: background-tone
 * separation only, single accent, monospace file names. Includes a filter box
 * that narrows the tree as you type. Read-only — Cody writes, the user reads.
 */
import { useEffect, useMemo, useState } from 'react'

import { Tree, type NodeApi, type NodeRendererProps } from 'react-arborist'
import useMeasure from 'react-use-measure'

import { Button } from '@/components/ui/button'
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
import { getTree, type TreeEntry, type WorkspaceTree } from '@/lib/api/workspace'
import { cn } from '@/lib/utils'

export interface TreeItem {
  id: string
  name: string
  children?: TreeItem[]
}

/** Per-filetype icon tint — the same warm ladder the editor theme uses, so
 *  the tree reads scannable without turning into a rainbow. */
const FILE_TINTS: Record<string, string> = {
  js: '#99bd9c',
  jsx: '#99bd9c',
  ts: '#99bd9c',
  tsx: '#99bd9c',
  mjs: '#99bd9c',
  cjs: '#99bd9c',
  go: '#93b3a7',
  py: '#93b3a7',
  rs: '#93b3a7',
  css: '#9fb3bd',
  scss: '#9fb3bd',
  html: '#d0a97e',
  htm: '#d0a97e',
  json: '#c9b590',
  yml: '#c9b590',
  yaml: '#c9b590',
  toml: '#c9b590',
  md: '#cbbfb8',
  markdown: '#cbbfb8',
  svg: '#c9b590',
  png: '#c9b590',
  jpg: '#c9b590',
  jpeg: '#c9b590',
  webp: '#c9b590',
  gif: '#c9b590',
  ico: '#c9b590',
}

function fileTint(name: string): string | undefined {
  const dot = name.lastIndexOf('.')
  if (dot <= 0) return undefined
  return FILE_TINTS[name.slice(dot + 1).toLowerCase()]
}

/** Fold the flat, path-keyed entries into arborist's nested shape. */
export function buildTreeItems(entries: TreeEntry[]): TreeItem[] {
  interface Node {
    item: TreeItem
    dir: boolean
    kids: Map<string, Node>
  }
  const root: Node = { item: { id: '', name: '' }, dir: true, kids: new Map() }

  const ensure = (path: string, dir: boolean): Node => {
    let cur = root
    let prefix = ''
    for (const part of path.split('/')) {
      prefix = prefix ? `${prefix}/${part}` : part
      let next = cur.kids.get(part)
      if (!next) {
        next = { item: { id: prefix, name: part }, dir: false, kids: new Map() }
        cur.kids.set(part, next)
      }
      cur = next
      if (prefix !== path) cur.dir = true
    }
    if (dir) cur.dir = true
    return cur
  }

  for (const entry of entries) {
    const path = entry.path.replace(/^\/+/, '').replace(/\/+$/, '')
    if (path) ensure(path, entry.dir)
  }

  const materialize = (node: Node): TreeItem => {
    if (node.dir || node.kids.size > 0) {
      const kids = [...node.kids.values()].sort((a, b) => {
        const aDir = a.dir || a.kids.size > 0
        const bDir = b.dir || b.kids.size > 0
        if (aDir !== bDir) return aDir ? -1 : 1
        return a.item.name.localeCompare(b.item.name, undefined, { sensitivity: 'base' })
      })
      node.item.children = kids.map(materialize)
    }
    return node.item
  }
  return materialize(root).children ?? []
}

export function CodyTree({
  projectID,
  onOpen,
  selected,
  className,
}: {
  projectID?: string
  onOpen: (path: string) => void
  selected?: string | null
  className?: string
}) {
  const [data, setData] = useState<WorkspaceTree | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)
  const [filter, setFilter] = useState('')
  const [ref, bounds] = useMeasure()

  useEffect(() => {
    const ctrl = new AbortController()
    let live = true
    setLoading(true)
    setError(null)
    getTree(projectID, ctrl.signal)
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .then((tree) => {
        if (!live) return
        setData(tree)
        setLoading(false)
      })
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .catch((err: unknown) => {
        if (!live || ctrl.signal.aborted) return
        setError(err instanceof Error ? err.message : 'Failed to load the tree')
        setLoading(false)
      })
    return () => {
      live = false
      ctrl.abort()
    }
  }, [projectID, nonce])

  const items = useMemo(() => (data ? buildTreeItems(data.entries) : []), [data])

  return (
    <section
      className={cn(
        'bg-surface-secondary flex h-full min-h-0 flex-col gap-2 rounded-lg p-2',
        className,
      )}
    >
      <div className="flex shrink-0 items-center gap-2 px-1 pt-1">
        <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
          Files
        </span>
        {data ? (
          <span className="text-muted-foreground ml-auto font-mono text-[10px]">
            {data.entries.length}
          </span>
        ) : null}
      </div>

      <input
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        placeholder="Filter files…"
        autoComplete="off"
        spellCheck={false}
        className="bg-surface-tertiary placeholder:text-muted-foreground shrink-0 rounded-md px-2.5 py-1.5 font-mono text-xs outline-none"
      />

      {loading ? (
        <CodyLoader variant="ring" size={24} className="m-auto py-4" />
      ) : error ? (
        <div className="flex flex-col items-center gap-2 p-4 text-center">
          <IconAlertCircle className="text-destructive size-4" />
          <p className="text-muted-foreground text-xs">{error}</p>
          <Button size="sm" variant="ghost" onClick={() => setNonce((n) => n + 1)}>
            <IconRefresh className="size-3.5" />
            <span>Retry</span>
          </Button>
        </div>
      ) : items.length === 0 ? (
        <p className="text-muted-foreground p-4 text-center text-xs">
          No files in this workspace yet.
        </p>
      ) : (
        <div ref={ref} className="min-h-0 flex-1">
          {bounds.height > 0 ? (
            <Tree<TreeItem>
              data={items}
              width={bounds.width}
              height={bounds.height}
              rowHeight={26}
              indent={12}
              openByDefault={false}
              searchTerm={filter}
              disableDrag
              disableDrop
              disableEdit
              selection={selected ?? undefined}
              onActivate={(node) => {
                if (node.isLeaf) onOpen(node.id)
              }}
            >
              {TreeRow}
            </Tree>
          ) : null}
        </div>
      )}

      {data?.truncated ? (
        <p className="text-muted-foreground shrink-0 px-1 pb-1 font-mono text-[10px]">
          tree truncated — not every file is listed
        </p>
      ) : null}
    </section>
  )
}

function TreeRow({ node, style, dragHandle }: NodeRendererProps<TreeItem>) {
  return (
    <div
      ref={dragHandle}
      style={style}
      onClick={() => handleClick(node)}
      className={cn(
        'flex h-full cursor-pointer items-center gap-1.5 rounded-md pr-2 text-sm',
        node.isSelected ? 'bg-surface-hover text-foreground' : 'hover:bg-surface-tertiary',
      )}
    >
      {node.isInternal ? (
        node.isOpen ? (
          <IconChevronDown className="text-muted-foreground size-3.5 shrink-0" />
        ) : (
          <IconChevronRight className="text-muted-foreground size-3.5 shrink-0" />
        )
      ) : (
        <span className="w-3.5 shrink-0" />
      )}
      {node.isInternal ? (
        node.isOpen ? (
          <IconFolderOpen className="text-muted-foreground size-4 shrink-0" />
        ) : (
          <IconFolder className="text-muted-foreground size-4 shrink-0" />
        )
      ) : (
        <IconFile
          className="text-muted-foreground size-4 shrink-0"
          style={{ color: fileTint(node.data.name) }}
        />
      )}
      <span className="truncate font-mono text-xs">{node.data.name}</span>
    </div>
  )
}

function handleClick(node: NodeApi<TreeItem>) {
  if (node.isInternal) node.toggle()
  else node.tree.props.onActivate?.(node)
}
