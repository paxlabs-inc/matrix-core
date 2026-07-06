'use client'

/**
 * Architect layout — the Replit-style information architecture in Matrix
 * chrome (design.body.md, per-tier reference target).
 *
 * Three panes: a chat rail (persistent transcript + checkpoint markers +
 * composer) | a tabbed center (Preview / Shell / Code / Diff / Spec /
 * Evidence with a ⌘K "search tools & files" palette) | a right Library file
 * tree. Maximum machinery: the live terminal, the durable spec viewer, the
 * full turn-in detail, and the git working-changes surface. Separation by
 * background tone only — no border strokes.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'

import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from '@/components/ui/resizable'
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import { CodyActivitySpine } from '@/components/matrix/cody/cody-activity'
import { CodyPreviewPane, CodyUndo } from '@/components/matrix/cody/cody-preview-undo'
import { CodyFileViewer } from '@/components/matrix/cody/cody-code-panes'
import { CodySpecViewer } from '@/components/matrix/cody/cody-architect-panes'
import { CodyTree } from '@/components/matrix/cody/cody-tree'
import { CodyDiffView } from '@/components/matrix/cody/cody-diff'
import { CodyShell } from '@/components/matrix/cody/cody-xterm'
import {
  DecisionCard,
  NeedsInput,
  RunComposer,
  TaskBoard,
  Transcript,
  TurninFeed,
  type ComposerChip,
  type WorkspaceActions,
} from '@/components/matrix/cody/cody-run-parts'
import { IconFile, IconSearch } from '@/components/matrix/cody/icons'
import { getTree } from '@/lib/api/cody'
import { cn } from '@/lib/utils'
import type { CodyRun } from '@/hooks/api/useCody'
import type { Tier } from '@/components/matrix/cody/tiering'

const CHIPS: ComposerChip[] = [
  { id: 'plan', label: 'Plan', prefix: '[plan first — show me the plan before building]' },
  { id: 'economy', label: 'Economy', prefix: '[be economical — smallest correct change]' },
]

type CenterTab = 'preview' | 'shell' | 'code' | 'diff' | 'spec' | 'evidence'

const CENTER_TABS: { key: CenterTab; label: string }[] = [
  { key: 'preview', label: 'Preview' },
  { key: 'shell', label: 'Shell' },
  { key: 'code', label: 'Code' },
  { key: 'diff', label: 'Diff' },
  { key: 'spec', label: 'Spec' },
  { key: 'evidence', label: 'Evidence' },
]

export function ArchitectLayout({
  run,
  tier,
  projectID,
  actions,
}: {
  run: CodyRun
  tier: Tier
  projectID?: string
  actions: WorkspaceActions
}) {
  const [tab, setTab] = useState<CenterTab>('preview')
  const [openFile, setOpenFile] = useState<string | null>(null)
  const [paletteOpen, setPaletteOpen] = useState(false)

  const openPath = useCallback((path: string) => {
    setOpenFile(path)
    setTab('code')
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'k' && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        setPaletteOpen((o) => !o)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  return (
    <>
      <ResizablePanelGroup direction="horizontal" className="h-full min-h-0 p-3">
        {/* ── Chat rail ── */}
        <ResizablePanel defaultSize={26} minSize={20} maxSize={38} className="pr-1.5">
          <div className="flex h-full min-h-0 flex-col gap-3">
            {run.goal ? (
              <h1 className="shrink-0 truncate text-sm font-medium" title={run.goal}>
                {run.goal}
              </h1>
            ) : null}

            {run.pending?.kind === 'decision' ? (
              <DecisionCard
                run={run}
                onAnswer={actions.onAnswer}
                blocking={tier.decisionBlocking}
              />
            ) : null}
            {run.pending && run.pending.kind !== 'decision' ? (
              <NeedsInput
                prompt={run.pending.prompt}
                onAnswer={(text) => actions.onAnswer({ text })}
              />
            ) : null}

            <CodyActivitySpine run={run} register={tier.register} />

            <div className="bg-surface-secondary flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto rounded-lg p-3">
              {run.tasks.length > 0 ? (
                <>
                  <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
                    Plan
                  </span>
                  <TaskBoard tasks={run.tasks} dense />
                </>
              ) : null}
              <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
                Conversation
              </span>
              <Transcript run={run} onAnswer={actions.onAnswer} />
              <CheckpointMarkers run={run} />
              <CodyUndo run={run} mode="architect" onRestored={actions.onRestored} />
            </div>

            <RunComposer run={run} actions={actions} chips={CHIPS} />
          </div>
        </ResizablePanel>

        <ResizableHandle className="bg-transparent" />

        {/* ── Tabbed center ── */}
        <ResizablePanel defaultSize={54} minSize={35} className="px-1.5">
          <div className="flex h-full min-h-0 flex-col gap-3">
            <div className="flex shrink-0 items-center gap-2">
              <div className="bg-surface-secondary flex gap-1 rounded-lg p-1">
                {CENTER_TABS.map((t) => (
                  <button
                    key={t.key}
                    type="button"
                    onClick={() => setTab(t.key)}
                    className={cn(
                      'rounded-md px-3 py-1.5 text-xs font-medium transition-colors',
                      tab === t.key
                        ? 'bg-surface-tertiary text-foreground'
                        : 'text-muted-foreground hover:bg-surface-hover',
                    )}
                  >
                    {t.label}
                  </button>
                ))}
              </div>
              <button
                type="button"
                onClick={() => setPaletteOpen(true)}
                className="bg-surface-secondary text-muted-foreground hover:bg-surface-tertiary ml-auto flex items-center gap-1.5 rounded-md px-2.5 py-1.5 font-mono text-[11px] transition-colors"
              >
                <IconSearch className="size-3.5" />
                <span>search files</span>
                <kbd className="bg-surface-hover rounded px-1 text-[10px]">⌘K</kbd>
              </button>
            </div>

            <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
              {tab === 'preview' ? (
                <CodyPreviewPane run={run} hero onRePreview={actions.onRePreview} />
              ) : tab === 'shell' ? (
                <CodyShell projectID={projectID} />
              ) : tab === 'code' ? (
                <div className="min-h-0 flex-1 overflow-y-auto">
                  {openFile ? (
                    <CodyFileViewer projectID={projectID} path={openFile} />
                  ) : (
                    <p className="text-muted-foreground p-4 text-sm">
                      Pick a file from the Library — or ⌘K to search.
                    </p>
                  )}
                </div>
              ) : tab === 'diff' ? (
                <CodyDiffView projectID={projectID} />
              ) : tab === 'spec' ? (
                <CodySpecViewer projectID={projectID} />
              ) : (
                <div className="min-h-0 flex-1 overflow-y-auto">
                  <TurninFeed run={run} showEvidence />
                  {run.tasks.every((t) => t.turnins.length === 0) ? (
                    <p className="text-muted-foreground p-4 text-sm">
                      Full turn-in detail appears here as tasks complete.
                    </p>
                  ) : null}
                </div>
              )}
            </div>
          </div>
        </ResizablePanel>

        <ResizableHandle className="bg-transparent" />

        {/* ── Library ── */}
        <ResizablePanel defaultSize={20} minSize={14} maxSize={30} className="pl-1.5">
          <CodyTree projectID={projectID} onOpen={openPath} selected={openFile} />
        </ResizablePanel>
      </ResizablePanelGroup>

      <FilePalette
        open={paletteOpen}
        onOpenChange={setPaletteOpen}
        projectID={projectID}
        onPick={openPath}
      />
    </>
  )
}

/** Checkpoint markers in the rail — "Checkpoint made", Replit-style. */
function CheckpointMarkers({ run }: { run: CodyRun }) {
  const created = run.snapshots.filter((s) => s.action === 'created')
  if (created.length === 0) return null
  return (
    <div className="flex flex-col gap-1">
      {created.map((s) => (
        <div
          key={`${s.seq}:${s.id}`}
          className="text-muted-foreground flex items-center gap-2 px-1 font-mono text-[10px]"
        >
          <span className="bg-pax size-1.5 shrink-0 rounded-full" />
          <span className="truncate">Checkpoint made{s.label ? ` — ${s.label}` : ''}</span>
        </div>
      ))}
    </div>
  )
}

/** ⌘K palette: fuzzy-search the workspace tree, open a file in the Code tab. */
function FilePalette({
  open,
  onOpenChange,
  projectID,
  onPick,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  projectID?: string
  onPick: (path: string) => void
}) {
  const [paths, setPaths] = useState<string[] | null>(null)

  useEffect(() => {
    if (!open || paths !== null) return
    const ctrl = new AbortController()
    getTree(projectID, ctrl.signal)
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .then((tree) => setPaths(tree.entries.filter((e) => !e.dir).map((e) => e.path)))
      // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
      .catch(() => setPaths([]))
    return () => ctrl.abort()
  }, [open, paths, projectID])

  const items = useMemo(() => paths ?? [], [paths])

  return (
    <CommandDialog open={open} onOpenChange={onOpenChange} title="Search files">
      <CommandInput placeholder="Search files…" />
      <CommandList>
        <CommandEmpty>{paths === null ? 'Loading…' : 'No files match.'}</CommandEmpty>
        <CommandGroup heading="Files">
          {items.map((p) => (
            <CommandItem
              key={p}
              value={p}
              onSelect={() => {
                onPick(p)
                onOpenChange(false)
              }}
            >
              <IconFile className="text-muted-foreground size-4" />
              <span className="truncate font-mono text-xs">{p}</span>
            </CommandItem>
          ))}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  )
}
