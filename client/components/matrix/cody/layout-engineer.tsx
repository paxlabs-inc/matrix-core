'use client'

/**
 * Engineer layout — the Leap.new-style information architecture in Matrix
 * chrome (design.body.md, per-tier reference target).
 *
 * Left rail: change history (checkpoints + accepted turn-ins) + the task-step
 * progress board + the "What's next?" composer with Scope/Thinking/Debug
 * chips. Main: a top-tab workspace — Preview | Code | Diff | Evidence — with
 * the file-tree + viewer code view and the @git-diff-view diff review. Bottom:
 * the BUILD / TESTS / LOG console strip fed by real verification evidence and
 * the run transcript. The engineering cockpit, technical register. Separation
 * by background tone only — no border strokes.
 */
import { useMemo, useState } from 'react'

import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from '@/components/ui/resizable'
import { CodyActivitySpine } from '@/components/matrix/cody/cody-activity'
import { CodyPreviewPane, CodyUndo } from '@/components/matrix/cody/cody-preview-undo'
import { CodyFileViewer } from '@/components/matrix/cody/cody-code-panes'
import { CodyTree } from '@/components/matrix/cody/cody-tree'
import { CodyDiffView } from '@/components/matrix/cody/cody-diff'
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
import { IconCheck, IconAlertCircle } from '@/components/matrix/cody/icons'
import { cn } from '@/lib/utils'
import type { CodyRun, CodyEvidence } from '@/hooks/api/useCody'
import type { Tier } from '@/components/matrix/cody/tiering'

const CHIPS: ComposerChip[] = [
  { id: 'scope', label: 'Scope', prefix: '[scope this precisely — no extras]' },
  { id: 'thinking', label: 'Thinking', prefix: '[think it through before acting]' },
  { id: 'debug', label: 'Debug', prefix: '[debug]' },
]

type MainTab = 'preview' | 'code' | 'diff' | 'evidence'

type ConsoleTab = 'build' | 'tests' | 'log'

export function EngineerLayout({
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
  const [tab, setTab] = useState<MainTab>('preview')
  const [openFile, setOpenFile] = useState<string | null>(null)
  const [consoleTab, setConsoleTab] = useState<ConsoleTab>('build')
  const [consoleOpen, setConsoleOpen] = useState(true)

  return (
    <ResizablePanelGroup direction="horizontal" className="h-full min-h-0 p-3">
      {/* ── Left rail: history + progress + What's next ── */}
      <ResizablePanel defaultSize={26} minSize={20} maxSize={38} className="pr-1.5">
        <div className="flex h-full min-h-0 flex-col gap-3">
          {run.goal ? (
            <h1 className="shrink-0 truncate text-sm font-medium" title={run.goal}>
              {run.goal}
            </h1>
          ) : null}

          {run.pending?.kind === 'decision' ? (
            <DecisionCard run={run} onAnswer={actions.onAnswer} blocking={tier.decisionBlocking} />
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
                  Progress
                </span>
                <TaskBoard tasks={run.tasks} dense />
              </>
            ) : null}
            <span className="text-muted-foreground font-mono text-[10px] tracking-wide uppercase">
              Conversation
            </span>
            <Transcript run={run} onAnswer={actions.onAnswer} />
            <CodyUndo run={run} mode="engineer" onRestored={actions.onRestored} />
          </div>

          <div className="shrink-0">
            <span className="text-muted-foreground mb-1 block px-1 font-mono text-[10px] tracking-wide uppercase">
              What&apos;s next?
            </span>
            <RunComposer run={run} actions={actions} chips={CHIPS} />
          </div>
        </div>
      </ResizablePanel>

      <ResizableHandle className="bg-transparent" />

      {/* ── Main: top-tab workspace + bottom console ── */}
      <ResizablePanel defaultSize={74} minSize={45} className="pl-1.5">
        <div className="flex h-full min-h-0 flex-col gap-3">
          <div className="flex shrink-0 items-center gap-2">
            <TopTabs tab={tab} onSelect={setTab} />
          </div>

          <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
            {tab === 'preview' ? (
              <CodyPreviewPane run={run} hero onRePreview={actions.onRePreview} />
            ) : tab === 'code' ? (
              <div className="flex min-h-0 flex-1 gap-3">
                <div className="w-64 shrink-0">
                  <CodyTree projectID={projectID} onOpen={setOpenFile} selected={openFile} />
                </div>
                <div className="min-w-0 flex-1 overflow-y-auto">
                  {openFile ? (
                    <CodyFileViewer projectID={projectID} path={openFile} />
                  ) : (
                    <p className="text-muted-foreground p-4 text-sm">Select a file to read it.</p>
                  )}
                </div>
              </div>
            ) : tab === 'diff' ? (
              <CodyDiffView projectID={projectID} />
            ) : (
              <div className="min-h-0 flex-1 overflow-y-auto">
                <TurninFeed run={run} showEvidence={tier.turninEvidence} />
                {run.tasks.every((t) => t.turnins.length === 0) ? (
                  <p className="text-muted-foreground p-4 text-sm">
                    Turn-ins with verification evidence appear here as tasks complete.
                  </p>
                ) : null}
              </div>
            )}
          </div>

          <ConsoleStrip
            run={run}
            tab={consoleTab}
            open={consoleOpen}
            onSelect={(t) => {
              setConsoleTab(t)
              setConsoleOpen(true)
            }}
            onToggle={() => setConsoleOpen((o) => !o)}
          />
        </div>
      </ResizablePanel>
    </ResizablePanelGroup>
  )
}

function TopTabs({ tab, onSelect }: { tab: MainTab; onSelect: (t: MainTab) => void }) {
  const tabs: { key: MainTab; label: string }[] = [
    { key: 'preview', label: 'Preview' },
    { key: 'code', label: 'Code' },
    { key: 'diff', label: 'Diff' },
    { key: 'evidence', label: 'Evidence' },
  ]
  return (
    <div className="bg-surface-secondary flex gap-1 rounded-lg p-1">
      {tabs.map((t) => (
        <button
          key={t.key}
          type="button"
          onClick={() => onSelect(t.key)}
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
  )
}

// ── Bottom console: BUILD / TESTS / LOG over real evidence ───────────────────

interface EvidenceRow {
  task: string
  ev: CodyEvidence
}

function isTestCommand(cmd: string): boolean {
  return /\btest\b|vitest|jest|pytest|go test/.test(cmd)
}

function ConsoleStrip({
  run,
  tab,
  open,
  onSelect,
  onToggle,
}: {
  run: CodyRun
  tab: ConsoleTab
  open: boolean
  onSelect: (t: ConsoleTab) => void
  onToggle: () => void
}) {
  const { build, tests } = useMemo(() => {
    const build: EvidenceRow[] = []
    const tests: EvidenceRow[] = []
    for (const t of run.tasks) {
      for (const ti of t.turnins) {
        for (const ev of ti.verification) {
          ;(isTestCommand(ev.command) ? tests : build).push({ task: t.title, ev })
        }
      }
    }
    return { build, tests }
  }, [run.tasks])

  const counts: Record<ConsoleTab, number> = {
    build: build.length,
    tests: tests.length,
    log: run.messages.length,
  }

  return (
    <div className="bg-surface-secondary flex shrink-0 flex-col rounded-lg">
      <div className="flex items-center gap-1 p-1">
        {(['build', 'tests', 'log'] as const).map((t) => (
          <button
            key={t}
            type="button"
            onClick={() => onSelect(t)}
            className={cn(
              'rounded-md px-2.5 py-1 font-mono text-[10px] tracking-wide uppercase transition-colors',
              open && tab === t
                ? 'bg-surface-tertiary text-foreground'
                : 'text-muted-foreground hover:bg-surface-hover',
            )}
          >
            {t}
            {counts[t] > 0 ? <span className="text-muted-foreground ml-1">{counts[t]}</span> : null}
          </button>
        ))}
        <button
          type="button"
          onClick={onToggle}
          className="text-muted-foreground hover:text-foreground ml-auto px-2 py-1 font-mono text-[10px] uppercase transition-colors"
        >
          {open ? 'hide' : 'show'}
        </button>
      </div>
      {open ? (
        <div className="max-h-44 min-h-0 overflow-y-auto px-3 pb-3">
          {tab === 'log' ? (
            run.messages.length === 0 ? (
              <ConsoleEmpty>Cody&apos;s narration appears here as it works.</ConsoleEmpty>
            ) : (
              <div className="flex flex-col gap-1">
                {run.messages.map((m, i) => (
                  <p key={i} className="font-mono text-xs">
                    {m.text}
                  </p>
                ))}
              </div>
            )
          ) : (
            <EvidenceList
              rows={tab === 'build' ? build : tests}
              empty={
                tab === 'build'
                  ? 'Build verification appears here as tasks are checked.'
                  : 'Test runs appear here as tasks are checked.'
              }
            />
          )}
        </div>
      ) : null}
    </div>
  )
}

function EvidenceList({ rows, empty }: { rows: EvidenceRow[]; empty: string }) {
  if (rows.length === 0) return <ConsoleEmpty>{empty}</ConsoleEmpty>
  return (
    <div className="flex flex-col gap-1">
      {rows.map(({ task, ev }, i) => (
        <div key={i} className="flex items-center gap-2 font-mono text-xs">
          {ev.exit === 0 ? (
            <IconCheck className="text-pax size-3.5 shrink-0" />
          ) : (
            <IconAlertCircle className="text-destructive size-3.5 shrink-0" />
          )}
          <span className="truncate">{ev.command}</span>
          <span className="text-muted-foreground ml-auto shrink-0 truncate text-[10px]">
            {task}
          </span>
        </div>
      ))}
    </div>
  )
}

function ConsoleEmpty({ children }: { children: React.ReactNode }) {
  return <p className="text-muted-foreground py-1 text-xs">{children}</p>
}
