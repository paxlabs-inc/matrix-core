'use client'

/**
 * Artifact/action cards (NEO-WORKBENCH req 3) — bolt.diy's single best idea,
 * folded over Neo's REAL event stream. Each turn's workbench-relevant steps
 * (file writes/edits/moves + commands) group into one collapsible card in the
 * chat rail: header from Neo's narration, one row per action with a live
 * status icon driven by the daemon's real running→done/failed transitions
 * (tool.step start/end pairs — never synthesized client-side timing).
 * Clicking a file row opens it in the workbench Code view; a command row
 * reveals the terminal panel. Cards rebuild identically from the durable
 * trace on reopen because the fold is pure over the same NeoStep list
 * buildTaskFromTrace produces.
 */
import { useMemo, useState } from 'react'

import type { NeoStep } from '@/hooks/api/useChat'
import { CodyLoader } from '@/components/matrix/cody/loaders'

export type ArtifactRowStatus = 'running' | 'complete' | 'failed'

export interface ArtifactRow {
  id: string
  kind: 'file' | 'command'
  /** The file path (file rows) or the command line (command rows). */
  label: string
  /** The action verb for file rows (write/edit/move/delete). */
  action?: string
  status: ArtifactRowStatus
}

export interface ArtifactCardGroup {
  id: string
  /** Neo's narration preceding this batch of work (the card header). */
  header: string
  rows: ArtifactRow[]
}

/** Editor actions that count as WORK (renders a row). Inspection steps
 *  (read/list/search) stay off the card — the card shows what Neo changed
 *  and ran, mirroring bolt's artifact rows. */
const fileActions = new Set(['write', 'edit', 'move', 'delete'])

function rowStatus(step: NeoStep): ArtifactRowStatus {
  if (step.running) return 'running'
  return step.ok ? 'complete' : 'failed'
}

/** Pure fold: group each turn's workbench steps into artifact cards. A
 *  narration step closes the current group and seeds the next header. Rows
 *  keep step ids, so live upserts (running→done on the same id) re-fold into
 *  the same row with the new status. */
export function groupArtifactCards(steps: NeoStep[]): ArtifactCardGroup[] {
  const groups: ArtifactCardGroup[] = []
  let header = ''
  let current: ArtifactCardGroup | null = null
  for (const step of steps) {
    if (step.kind === 'narration') {
      if (step.text?.trim()) header = step.text.trim()
      current = null
      continue
    }
    let row: ArtifactRow | null = null
    if (step.kind === 'editor' && step.path && fileActions.has(step.action ?? '')) {
      row = {
        id: step.id,
        kind: 'file',
        label: step.path,
        action: step.action,
        status: rowStatus(step),
      }
    } else if (step.kind === 'terminal' && step.command) {
      row = { id: step.id, kind: 'command', label: step.command, status: rowStatus(step) }
    }
    if (!row) continue
    if (!current) {
      current = { id: `card-${step.id}`, header: header || 'Working on it', rows: [] }
      groups.push(current)
    }
    current.rows.push(row)
  }
  return groups
}

function StatusIcon({ status }: { status: ArtifactRowStatus }) {
  if (status === 'running') return <CodyLoader variant="dots" />
  if (status === 'failed')
    return (
      <span aria-label="failed" className="text-destructive font-mono text-xs">
        ×
      </span>
    )
  return (
    <span aria-label="complete" className="font-mono text-xs text-[var(--ring)]">
      ✓
    </span>
  )
}

export function ArtifactCard({
  group,
  onOpenFile,
  onRevealTerminal,
}: {
  group: ArtifactCardGroup
  onOpenFile: (path: string) => void
  onRevealTerminal: () => void
}) {
  const [collapsed, setCollapsed] = useState(false)
  const running = group.rows.some((r) => r.status === 'running')
  const failed = group.rows.some((r) => r.status === 'failed')
  return (
    <div data-testid="artifact-card" className="bg-surface-secondary w-full rounded-xl">
      <button
        className="flex w-full items-center gap-2 px-3 py-2.5 text-left"
        aria-expanded={!collapsed}
        onClick={() => setCollapsed((c) => !c)}
      >
        <span className="min-w-0 flex-1 truncate text-sm font-medium">{group.header}</span>
        <span className="text-muted-foreground font-mono text-[10px] uppercase">
          {running ? 'working' : failed ? 'had trouble' : 'done'}
        </span>
      </button>
      {!collapsed ? (
        <ul className="flex flex-col gap-0.5 px-2 pb-2">
          {group.rows.map((row) => (
            <li key={row.id}>
              <button
                data-row-kind={row.kind}
                data-row-status={row.status}
                onClick={() => (row.kind === 'file' ? onOpenFile(row.label) : onRevealTerminal())}
                className="hover:bg-surface-hover flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left"
              >
                <StatusIcon status={row.status} />
                {row.kind === 'file' ? (
                  <>
                    <span className="text-muted-foreground text-xs capitalize">
                      {row.action ?? 'write'}
                    </span>
                    <span className="min-w-0 flex-1 truncate font-mono text-xs">{row.label}</span>
                  </>
                ) : (
                  <span className="min-w-0 flex-1 truncate font-mono text-xs">$ {row.label}</span>
                )}
              </button>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  )
}

/** All cards for a task's steps, memoized on the steps array. */
export function ArtifactCards({
  steps,
  onOpenFile,
  onRevealTerminal,
}: {
  steps: NeoStep[]
  onOpenFile: (path: string) => void
  onRevealTerminal: () => void
}) {
  const groups = useMemo(() => groupArtifactCards(steps), [steps])
  if (groups.length === 0) return null
  return (
    <div className="flex w-full flex-col gap-2">
      {groups.map((g) => (
        <ArtifactCard
          key={g.id}
          group={g}
          onOpenFile={onOpenFile}
          onRevealTerminal={onRevealTerminal}
        />
      ))}
    </div>
  )
}
