'use client'

/**
 * Shared run parts — the tier-agnostic pieces every layout composes: the waved
 * task board, turn-in evidence cards, the blocking decision/needs-input cards,
 * the persistent transcript, and the ONE context-aware composer (steer /
 * answer / continue — never a dead disabled box). Extracted from the old
 * single-layout workspace so the three per-tier layouts (Prototype/Engineer/
 * Architect) compose them without duplication. Layers separate by background
 * tone — no border strokes.
 */
import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconCheck, IconAlertCircle, IconClock } from '@/components/matrix/cody/icons'
import { cn } from '@/lib/utils'
import type { AnswerVerdict } from '@/lib/api/cody'
import type { CodyRun, CodyTask, CodyTurn, CodyTurnin } from '@/hooks/api/useCody'
import type { NewRunInput } from '@/components/matrix/cody/cody-new-run'

export interface WorkspaceActions {
  onStartRun: (input: NewRunInput) => void
  onSteer: (text: string) => void
  onAnswer: (answer: { text?: string; verdict?: AnswerVerdict }) => void
  /** Re-dispatch the same conversation: resumes the durable plan (req 3.2). */
  onContinue: (text: string) => void
  onRePreview: () => void
  onRestored?: () => void
}

// ── Task board ────────────────────────────────────────────────────────────────

export function TaskBoard({ tasks, dense }: { tasks: CodyTask[]; dense?: boolean }) {
  const waves = groupWaves(tasks)
  return (
    <section
      className={cn(
        'flex flex-col gap-3 rounded-lg',
        dense ? 'bg-transparent p-0' : 'bg-surface-secondary p-4',
      )}
    >
      {!dense ? (
        <h2 className="text-muted-foreground font-mono text-xs tracking-wide uppercase">Plan</h2>
      ) : null}
      <div className="flex flex-col gap-4">
        {waves.map(({ wave, items }) => (
          <div key={wave} className="flex flex-col gap-1.5">
            <span className="text-muted-foreground font-mono text-[10px]">wave {wave}</span>
            {items.map((t) => (
              <div
                key={t.id}
                className={cn(
                  'flex items-center gap-2 rounded-md px-3 py-2',
                  dense ? 'bg-surface-secondary' : 'bg-surface-tertiary',
                )}
              >
                <TaskStatusIcon status={t.status} />
                <span className="truncate text-sm">{t.title}</span>
                {t.attempt > 1 ? (
                  <span className="text-muted-foreground ml-auto font-mono text-[10px]">
                    attempt {t.attempt}
                  </span>
                ) : null}
              </div>
            ))}
          </div>
        ))}
      </div>
    </section>
  )
}

export function TaskStatusIcon({ status }: { status: CodyTask['status'] }) {
  if (status === 'done') return <IconCheck className="text-pax size-4 shrink-0" />
  if (status === 'failed') return <IconAlertCircle className="text-destructive size-4 shrink-0" />
  if (status === 'in_progress') return <CodyLoader variant="ring" size={16} />
  return <IconClock className="text-muted-foreground size-4 shrink-0" />
}

export function groupWaves(tasks: CodyTask[]): { wave: number; items: CodyTask[] }[] {
  const byWave = new Map<number, CodyTask[]>()
  for (const t of tasks) {
    const list = byWave.get(t.wave) ?? []
    list.push(t)
    byWave.set(t.wave, list)
  }
  return [...byWave.entries()].sort((a, b) => a[0] - b[0]).map(([wave, items]) => ({ wave, items }))
}

// ── Turn-in feed ──────────────────────────────────────────────────────────────

export function TurninFeed({ run, showEvidence }: { run: CodyRun; showEvidence: boolean }) {
  const turnins = run.tasks.flatMap((t) => t.turnins.map((ti) => ({ task: t, ti })))
  if (turnins.length === 0) return null
  return (
    <section className="flex flex-col gap-2">
      {turnins.map(({ task, ti }, i) => (
        <TurninCard
          key={`${task.id}:${ti.attempt}:${i}`}
          task={task}
          ti={ti}
          showEvidence={showEvidence}
        />
      ))}
    </section>
  )
}

export function TurninCard({
  task,
  ti,
  showEvidence,
}: {
  task: CodyTask
  ti: CodyTurnin
  showEvidence: boolean
}) {
  return (
    <div className="bg-surface-secondary flex flex-col gap-2 rounded-lg p-4">
      <div className="flex items-center gap-2">
        <span className="text-sm font-medium">{task.title}</span>
        <span className="text-muted-foreground ml-auto font-mono text-[10px] uppercase">
          {ti.status}
        </span>
      </div>
      {ti.summary ? <p className="text-muted-foreground text-sm">{ti.summary}</p> : null}

      {ti.changes.length > 0 ? (
        <ul className="flex flex-col gap-1">
          {ti.changes.map((c, i) => (
            <li key={i} className="flex items-center gap-2 font-mono text-xs">
              <span className="text-muted-foreground w-14 shrink-0 uppercase">{c.kind}</span>
              <span className="truncate">{c.path}</span>
            </li>
          ))}
        </ul>
      ) : null}

      {showEvidence && ti.verification.length > 0 ? (
        <div className="flex flex-col gap-1">
          <span className="text-muted-foreground font-mono text-[10px] uppercase">
            verification
          </span>
          {ti.verification.map((ev, i) => (
            <div key={i} className="flex items-center gap-2 font-mono text-xs">
              {ev.exit === 0 ? (
                <IconCheck className="text-pax size-3.5" />
              ) : (
                <IconAlertCircle className="text-destructive size-3.5" />
              )}
              <span className="truncate">{ev.command}</span>
            </div>
          ))}
        </div>
      ) : null}

      {ti.gaps.length > 0 ? (
        <div className="flex flex-col gap-1">
          <span className="text-muted-foreground font-mono text-[10px] uppercase">honest gaps</span>
          {ti.gaps.map((g, i) => (
            <p key={i} className="text-sm">
              {g}
            </p>
          ))}
        </div>
      ) : null}
    </div>
  )
}

// ── Needs-input + decision cards ──────────────────────────────────────────────

export function NeedsInput({
  prompt,
  onAnswer,
}: {
  prompt: string
  onAnswer: (text: string) => void
}) {
  const [text, setText] = useState('')
  return (
    <section className="bg-surface-dialog flex shrink-0 flex-col gap-3 rounded-lg p-4">
      <p className="text-sm">{prompt}</p>
      <Textarea
        value={text}
        rows={2}
        placeholder="Your answer…"
        onChange={(e) => setText(e.target.value)}
      />
      <div className="flex justify-end">
        <Button
          size="sm"
          onClick={() => {
            if (text.trim()) onAnswer(text.trim())
          }}
          disabled={!text.trim()}
        >
          Send answer
        </Button>
      </div>
    </section>
  )
}

export function DecisionCard({
  run,
  onAnswer,
  blocking,
}: {
  run: CodyRun
  onAnswer: (answer: { text?: string; verdict?: AnswerVerdict }) => void
  blocking: boolean
}) {
  const [override, setOverride] = useState('')
  const [showOverride, setShowOverride] = useState(false)
  const stack = run.stackDecision
  const design = run.designDecision

  return (
    <section className="bg-surface-dialog flex shrink-0 flex-col gap-3 rounded-lg p-4">
      <div className="flex items-center gap-2">
        <span className="text-sm font-medium">
          {stack ? 'Stack decision' : design ? 'Design direction' : 'Decision'}
        </span>
        {!blocking ? (
          <span className="text-muted-foreground ml-auto font-mono text-[10px] uppercase">
            informational
          </span>
        ) : null}
      </div>

      {stack ? <StackSummary run={run} /> : null}
      {design ? <DesignSummary run={run} /> : null}
      {run.pending?.prompt ? (
        <p className="text-muted-foreground text-sm">{run.pending.prompt}</p>
      ) : null}

      {blocking ? (
        <>
          {showOverride ? (
            <Textarea
              value={override}
              rows={2}
              placeholder={stack ? 'Use this stack instead…' : 'Use this design instead…'}
              onChange={(e) => setOverride(e.target.value)}
            />
          ) : null}
          <div className="flex items-center justify-end gap-2">
            {showOverride ? (
              <Button
                size="sm"
                onClick={() =>
                  onAnswer({
                    verdict: { decision: 'override', payload: override.trim() },
                    text: override.trim(),
                  })
                }
                disabled={!override.trim()}
              >
                Submit override
              </Button>
            ) : (
              <Button size="sm" variant="ghost" onClick={() => setShowOverride(true)}>
                Override
              </Button>
            )}
            <Button size="sm" onClick={() => onAnswer({ verdict: { decision: 'approve' } })}>
              <IconCheck className="size-3.5" />
              <span>Approve</span>
            </Button>
          </div>
        </>
      ) : null}
    </section>
  )
}

function StackSummary({ run }: { run: CodyRun }) {
  const d = run.stackDecision
  if (!d) return null
  const chosen = d.candidates[d.pick]
  return (
    <div className="flex flex-col gap-2">
      {chosen ? <span className="font-mono text-sm">{chosen.name}</span> : null}
      {d.rationale ? <p className="text-muted-foreground text-sm">{d.rationale}</p> : null}
    </div>
  )
}

function DesignSummary({ run }: { run: CodyRun }) {
  const d = run.designDecision
  if (!d) return null
  return (
    <div className="flex flex-col gap-2">
      <span className="font-mono text-sm">{d.summary ?? d.styleDirection}</span>
      {d.rationale ? <p className="text-muted-foreground text-sm">{d.rationale}</p> : null}
    </div>
  )
}

// ── Context-aware composer ────────────────────────────────────────────────────

/**
 * The composer's action follows the run state (req 3.1) — it is NEVER a dead
 * disabled box:
 *   running      -> steer (folded at the next boundary)
 *   needs_input  -> answer the open question
 *   stopped/completed/failed -> continue (re-dispatch resumes the same
 *                   durable plan over this conversation)
 */
export type ComposerIntent = 'steer' | 'answer' | 'continue'

export function composerIntent(status: CodyRun['status']): ComposerIntent {
  if (status === 'running') return 'steer'
  if (status === 'needs_input') return 'answer'
  return 'continue'
}

const COMPOSER_COPY: Record<ComposerIntent, { placeholder: string; action: string }> = {
  steer: { placeholder: 'Steer the run…', action: 'Steer' },
  answer: { placeholder: 'Answer Cody…', action: 'Answer' },
  continue: { placeholder: 'Tell Cody what to do next…', action: 'Continue' },
}

export interface ComposerChip {
  id: string
  label: string
  /** Prefixed to the message so the engine sees the focus, e.g. "[debug]". */
  prefix: string
}

export function RunComposer({
  run,
  actions,
  chips,
  placeholder,
}: {
  run: CodyRun
  actions: WorkspaceActions
  chips?: ComposerChip[]
  placeholder?: string
}) {
  const [text, setText] = useState('')
  const [chip, setChip] = useState<string | null>(null)
  const intent = composerIntent(run.status)
  const submit = () => {
    const raw = text.trim()
    if (!raw) return
    const prefix = chips?.find((c) => c.id === chip)?.prefix
    const t = prefix ? `${prefix} ${raw}` : raw
    if (intent === 'steer') actions.onSteer(t)
    else if (intent === 'answer') actions.onAnswer({ text: t })
    else actions.onContinue(t)
    setText('')
    setChip(null)
  }
  return (
    <div className="bg-surface-tertiary flex flex-col gap-2 rounded-lg p-2">
      {chips && chips.length > 0 ? (
        <div className="flex items-center gap-1">
          {chips.map((c) => (
            <button
              key={c.id}
              type="button"
              onClick={() => setChip((cur) => (cur === c.id ? null : c.id))}
              className={cn(
                'rounded-md px-2 py-1 font-mono text-[10px] tracking-wide uppercase transition-colors',
                chip === c.id
                  ? 'bg-pax text-primary-foreground'
                  : 'bg-surface-secondary text-muted-foreground hover:bg-surface-hover',
              )}
            >
              {c.label}
            </button>
          ))}
        </div>
      ) : null}
      <Textarea
        value={text}
        rows={2}
        placeholder={placeholder ?? COMPOSER_COPY[intent].placeholder}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault()
            submit()
          }
        }}
        className="border-0 bg-transparent shadow-none focus-visible:ring-0"
      />
      <div className="flex items-center justify-between">
        <span className="text-muted-foreground font-mono text-[10px]">
          {intent === 'continue' ? 'resumes this conversation' : ''}
        </span>
        <Button size="sm" onClick={submit} disabled={!text.trim()}>
          {COMPOSER_COPY[intent].action}
        </Button>
      </div>
    </div>
  )
}

// ── Persistent transcript ─────────────────────────────────────────────────────

/**
 * Both sides + Cody's inline questions, rebuilt identically from the durable
 * trace on reopen (req 3.3, 4.2). The open question (while the run awaits
 * input) carries inline approve for decisions.
 */
export function Transcript({
  run,
  onAnswer,
}: {
  run: CodyRun
  onAnswer: (answer: { text?: string; verdict?: AnswerVerdict }) => void
}) {
  if (run.transcript.length === 0) {
    return (
      <div className="flex min-h-0 flex-1 flex-col gap-2 overflow-y-auto">
        <p className="text-muted-foreground text-xs">The conversation appears here.</p>
      </div>
    )
  }
  const lastQuestionSeq = [...run.transcript].reverse().find((t) => t.role === 'question')?.seq
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2 overflow-y-auto">
      {run.transcript.map((turn) => (
        <TranscriptTurn
          key={`${turn.role}:${turn.seq}`}
          turn={turn}
          open={run.status === 'needs_input' && turn.seq === lastQuestionSeq}
          decision={run.pending?.kind === 'decision'}
          onAnswer={onAnswer}
        />
      ))}
    </div>
  )
}

function TranscriptTurn({
  turn,
  open,
  decision,
  onAnswer,
}: {
  turn: CodyTurn
  open: boolean
  decision: boolean
  onAnswer: (answer: { text?: string; verdict?: AnswerVerdict }) => void
}) {
  if (turn.role === 'user') {
    return (
      <div className="bg-surface-hover ml-6 rounded-md px-3 py-2 text-sm">
        {turn.kind === 'steer' ? (
          <span className="text-muted-foreground mr-1.5 font-mono text-[10px] uppercase">
            steer
          </span>
        ) : null}
        {turn.decision && !turn.text ? (
          <span className="font-mono text-xs uppercase">{turn.decision}</span>
        ) : (
          turn.text
        )}
      </div>
    )
  }
  if (turn.role === 'question') {
    return (
      <div className="bg-surface-dialog flex flex-col gap-2 rounded-md px-3 py-2">
        <p className="text-sm">{turn.text}</p>
        {open && decision ? (
          <div className="flex items-center justify-end gap-2">
            <span className="text-muted-foreground mr-auto text-[11px]">
              or type a different direction below
            </span>
            <Button size="sm" onClick={() => onAnswer({ verdict: { decision: 'approve' } })}>
              <IconCheck className="size-3.5" />
              <span>Approve</span>
            </Button>
          </div>
        ) : null}
      </div>
    )
  }
  return <div className="bg-surface-tertiary rounded-md px-3 py-2 text-sm">{turn.text}</div>
}
