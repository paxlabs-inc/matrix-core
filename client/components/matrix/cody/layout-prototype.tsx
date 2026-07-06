'use client'

/**
 * Prototype layout — the Emergent-style information architecture in Matrix
 * chrome (design.body.md, per-tier reference target).
 *
 * Entry: an atmosphere hero — "What will you build today?" — with an
 * intent-tabbed composer (Full Stack / Mobile / Landing / Brainstorm) and the
 * spec-adoption hatch. Running: a chat-left + live App-Preview-right split
 * with a run-details strip and one-button undo; the single "show me the code"
 * escape hatch opens the read-only code pane. Vibe-first, preview-as-hero,
 * minimal machinery, outcome-register copy. Separation by background tone
 * only — no border strokes, no gradients, no glow.
 */
import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from '@/components/ui/resizable'
import { CodyActivitySpine } from '@/components/matrix/cody/cody-activity'
import { CodyPreviewPane, CodyUndo } from '@/components/matrix/cody/cody-preview-undo'
import { CodyFileViewer } from '@/components/matrix/cody/cody-code-panes'
import { CodyTree } from '@/components/matrix/cody/cody-tree'
import { CodyNewRun, type NewRunInput } from '@/components/matrix/cody/cody-new-run'
import {
  DecisionCard,
  NeedsInput,
  RunComposer,
  Transcript,
  type WorkspaceActions,
} from '@/components/matrix/cody/cody-run-parts'
import { IconPlay, IconCode } from '@/components/matrix/cody/icons'
import { cn } from '@/lib/utils'
import type { CodyRun } from '@/hooks/api/useCody'
import type { Tier } from '@/components/matrix/cody/tiering'

// ── Entry hero ────────────────────────────────────────────────────────────────

type Intent = 'full_stack' | 'mobile' | 'landing' | 'brainstorm'

const INTENTS: { id: Intent; label: string; hint: string }[] = [
  { id: 'full_stack', label: 'Full Stack App', hint: 'A recipe app where I can save favorites…' },
  { id: 'mobile', label: 'Mobile App', hint: 'A mobile-first habit tracker with streaks…' },
  { id: 'landing', label: 'Landing Page', hint: 'A landing page for my coffee subscription…' },
  { id: 'brainstorm', label: 'Brainstorm', hint: 'Help me shape an idea for a booking tool…' },
]

const INTENT_PREFIX: Record<Intent, string> = {
  full_stack: '',
  mobile: 'A mobile-first build:',
  landing: 'A landing page:',
  brainstorm: 'Brainstorm with me before building:',
}

export function PrototypeHero({
  projectID,
  onStart,
}: {
  projectID?: string
  onStart: (input: NewRunInput) => void
}) {
  const [intent, setIntent] = useState<Intent>('full_stack')
  const [text, setText] = useState('')
  const [adopting, setAdopting] = useState(false)

  const submit = () => {
    const t = text.trim()
    if (!t) return
    const prefix = INTENT_PREFIX[intent]
    onStart({ message: prefix ? `${prefix} ${t}` : t })
  }

  if (adopting) {
    return (
      <div className="flex h-full min-h-0 flex-col">
        <div className="p-4">
          <Button size="sm" variant="ghost" onClick={() => setAdopting(false)}>
            Back
          </Button>
        </div>
        <CodyNewRun projectID={projectID} outcome onStart={onStart} />
      </div>
    )
  }

  const active = INTENTS.find((i) => i.id === intent) ?? INTENTS[0]

  return (
    <div className="flex h-full min-h-0 flex-col items-center justify-center gap-6 p-6">
      <h1 className="text-center text-3xl font-medium tracking-tight">
        What will you build today?
      </h1>

      <div className="flex w-full max-w-2xl flex-col gap-0">
        <div className="flex gap-1 px-1">
          {INTENTS.map((i) => (
            <button
              key={i.id}
              type="button"
              onClick={() => setIntent(i.id)}
              className={cn(
                'rounded-t-lg px-3 py-2 text-xs font-medium transition-colors',
                intent === i.id
                  ? 'bg-surface-secondary text-foreground'
                  : 'text-muted-foreground hover:bg-surface-tertiary',
              )}
            >
              {i.label}
            </button>
          ))}
        </div>
        <div className="bg-surface-secondary flex flex-col gap-2 rounded-lg rounded-tl-none p-3">
          <Textarea
            value={text}
            autoFocus
            rows={4}
            placeholder={active.hint}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit()
            }}
            className="border-0 bg-transparent shadow-none focus-visible:ring-0"
          />
          <div className="flex items-center justify-between">
            <button
              type="button"
              onClick={() => setAdopting(true)}
              className="text-muted-foreground hover:text-foreground font-mono text-[11px] transition-colors"
            >
              have a spec? adopt it
            </button>
            <Button onClick={submit} disabled={!text.trim()}>
              <IconPlay className="size-3.5" />
              <span>Build</span>
            </Button>
          </div>
        </div>
      </div>

      <p className="text-muted-foreground max-w-md text-center text-sm">
        Say it plainly. Cody plans it, builds it, and shows you a working preview.
      </p>
    </div>
  )
}

// ── Running layout: chat left, App Preview hero right ─────────────────────────

export function PrototypeLayout({
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
  const [showCode, setShowCode] = useState(false)
  const [openFile, setOpenFile] = useState<string | null>(null)

  return (
    <ResizablePanelGroup direction="horizontal" className="h-full min-h-0 p-3">
      <ResizablePanel defaultSize={38} minSize={28} className="flex min-h-0 flex-col gap-3 pr-1.5">
        {run.goal ? (
          <div className="flex shrink-0 items-start gap-3">
            <h1 className="min-w-0 flex-1 truncate text-base font-medium">{run.goal}</h1>
            <CodyUndo run={run} mode="prototype" onRestored={actions.onRestored} />
          </div>
        ) : null}

        {run.pending?.kind === 'decision' ? (
          <DecisionCard run={run} onAnswer={actions.onAnswer} blocking={tier.decisionBlocking} />
        ) : null}
        {run.pending && run.pending.kind !== 'decision' ? (
          <NeedsInput prompt={run.pending.prompt} onAnswer={(text) => actions.onAnswer({ text })} />
        ) : null}

        <CodyActivitySpine run={run} register={tier.register} />
        <Transcript run={run} onAnswer={actions.onAnswer} />
        <RunComposer run={run} actions={actions} />
      </ResizablePanel>

      <ResizableHandle className="bg-transparent" />

      <ResizablePanel defaultSize={62} minSize={35} className="flex min-h-0 flex-col gap-3 pl-1.5">
        <div className="flex shrink-0 items-center gap-2">
          <RunDetails run={run} />
          <button
            type="button"
            onClick={() => setShowCode((s) => !s)}
            className={cn(
              'ml-auto flex items-center gap-1.5 rounded-md px-2.5 py-1.5 font-mono text-[11px] transition-colors',
              showCode
                ? 'bg-surface-hover text-foreground'
                : 'bg-surface-secondary text-muted-foreground hover:bg-surface-tertiary',
            )}
          >
            <IconCode className="size-3.5" />
            <span>{showCode ? 'Back to the app' : 'Show me the code'}</span>
          </button>
        </div>

        {showCode ? (
          <div className="flex min-h-0 flex-1 gap-3">
            <div className="w-60 shrink-0">
              <CodyTree projectID={projectID} onOpen={setOpenFile} selected={openFile} />
            </div>
            <div className="min-w-0 flex-1 overflow-y-auto">
              {openFile ? (
                <CodyFileViewer projectID={projectID} path={openFile} />
              ) : (
                <p className="text-muted-foreground p-4 text-sm">Pick a file to read it.</p>
              )}
            </div>
          </div>
        ) : (
          <CodyPreviewPane run={run} hero onRePreview={actions.onRePreview} />
        )}
      </ResizablePanel>
    </ResizablePanelGroup>
  )
}

/** The run-details strip: honest facts only (mode, status, checkpoints). */
function RunDetails({ run }: { run: CodyRun }) {
  const checkpoints = run.snapshots.filter((s) => s.action === 'created').length
  return (
    <div className="bg-surface-secondary flex items-center gap-3 rounded-md px-3 py-1.5 font-mono text-[11px]">
      <span className="text-muted-foreground">
        status <span className="text-foreground">{run.status}</span>
      </span>
      {run.mode ? (
        <span className="text-muted-foreground">
          mode <span className="text-foreground">{run.mode}</span>
        </span>
      ) : null}
      <span className="text-muted-foreground">
        checkpoints <span className="text-foreground">{checkpoints}</span>
      </span>
    </div>
  )
}
