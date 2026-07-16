'use client'

/**
 * Structure — a composite of records in one of three shapes: list | table |
 * tree. Covers collections, filesystem trees, plan DAGs, the DOM, search
 * results. The shape is chosen by the agent; the renderer paints each shape
 * trustedly (a tree collapses, a table aligns columns), never raw JSON.
 */
import { useState } from 'react'
import { ChevronRight, ExternalLink } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { Structure as StructurePayload, StructureNode } from '@/lib/construct/types.gen'

function asHttpUrl(url: string): string {
  return /^https?:\/\//i.test(url) ? url : `https://${url}`
}

/* ----------------------------- list ----------------------------- */

function ListRow({ node }: { node: StructureNode }) {
  const cells = node.cells ? Object.entries(node.cells).filter(([, v]) => v) : []
  const body = (
    <>
      <span className="text-foreground line-clamp-1 block text-sm font-medium">
        {node.label || node.ref || node.id}
      </span>
      {cells.length > 0 && (
        <span className="text-muted-foreground mt-0.5 line-clamp-2 block text-xs leading-snug">
          {cells.map(([, v]) => v).join(' · ')}
        </span>
      )}
      {node.ref && (
        <span className="text-primary mt-1 flex items-center gap-1 font-mono text-[0.65rem]">
          <ExternalLink className="size-3 shrink-0" />
          <span className="truncate">{node.ref}</span>
        </span>
      )}
    </>
  )
  if (node.ref) {
    return (
      <a
        href={asHttpUrl(node.ref)}
        target="_blank"
        rel="noreferrer noopener"
        className="bg-popover hover:bg-muted block rounded-2xl p-3 transition-colors"
      >
        {body}
      </a>
    )
  }
  return <div className="bg-popover rounded-2xl p-3">{body}</div>
}

function ListView({ records }: { records: StructureNode[] }) {
  return (
    <div className="flex flex-col gap-2">
      {records.map((n, i) => (
        <ListRow key={n.id || i} node={n} />
      ))}
    </div>
  )
}

/* ----------------------------- table ---------------------------- */

function TableView({ columns, records }: { columns: string[]; records: StructureNode[] }) {
  const cols = columns.length > 0 ? columns : inferColumns(records)
  return (
    <div className="bg-foreground/[0.03] overflow-x-auto rounded-2xl">
      <table className="w-full text-left text-xs">
        <thead>
          <tr className="text-muted-foreground/80">
            {cols.map((c) => (
              <th key={c} className="px-3 py-2 font-mono text-[0.62rem] tracking-wide uppercase">
                {c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {records.map((n, i) => (
            <tr key={n.id || i} className="text-foreground/90 odd:bg-foreground/[0.02]">
              {cols.map((c) => (
                <td key={c} className="truncate px-3 py-2">
                  {n.cells?.[c] ?? (c === cols[0] ? n.label : '') ?? ''}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function inferColumns(records: StructureNode[]): string[] {
  const seen: string[] = []
  for (const n of records) {
    for (const k of Object.keys(n.cells ?? {})) if (!seen.includes(k)) seen.push(k)
  }
  return seen
}

/* ----------------------------- tree ----------------------------- */

function TreeNode({ node, depth }: { node: StructureNode; depth: number }) {
  const [open, setOpen] = useState(depth < 2)
  const children = node.children ?? []
  const hasChildren = children.length > 0
  return (
    <div>
      <div
        className="hover:bg-foreground/[0.04] flex items-center gap-1.5 rounded-lg py-1 pr-2 transition-colors"
        style={{ paddingLeft: `${depth * 14 + 4}px` }}
      >
        {hasChildren ? (
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            className="text-muted-foreground hover:text-foreground grid size-4 shrink-0 place-items-center"
          >
            <ChevronRight className={cn('size-3.5 transition-transform', open && 'rotate-90')} />
          </button>
        ) : (
          <span className="bg-foreground/20 ml-[0.4rem] size-1 shrink-0 rounded-full" />
        )}
        <span className="text-foreground/90 truncate font-mono text-[0.78rem]">
          {node.label || node.id}
        </span>
      </div>
      {hasChildren && open && (
        <div>
          {children.map((c, i) => (
            <TreeNode key={c.id || i} node={c} depth={depth + 1} />
          ))}
        </div>
      )}
    </div>
  )
}

function TreeView({ records }: { records: StructureNode[] }) {
  return (
    <div className="bg-foreground/[0.03] rounded-2xl p-1.5">
      {records.map((n, i) => (
        <TreeNode key={n.id || i} node={n} depth={0} />
      ))}
    </div>
  )
}

export function StructureView({ structure }: { structure: StructurePayload }) {
  if (structure.records.length === 0) return null
  switch (structure.shape) {
    case 'table':
      return <TableView columns={structure.columns ?? []} records={structure.records} />
    case 'tree':
      return <TreeView records={structure.records} />
    default:
      return <ListView records={structure.records} />
  }
}
