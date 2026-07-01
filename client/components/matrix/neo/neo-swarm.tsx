'use client'

/**
 * NeoSwarm — the live Agent Swarm card.
 *
 * Surfaced from the swarm.* / subagent.* SSE events when Neo fans a task out to
 * several task-scoped sub-agents that run CONCURRENTLY. Each sub-agent renders
 * as a row — avatar, name, its live activity, and an LED progress strip — and
 * expands into its OWN animated "Agent's Window" (the same NeoWorkspace viewport
 * as Neo's), since each sub-agent runs in its own isolated context. Once the
 * swarm settles a compact completed-avatar row caps it. Tone-only separation
 * (popover on card), single accent #004ced, green for "done".
 */
import { useState } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import { AlertTriangle, Check, ChevronDown, Loader2, MatrixIcon } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { NeoSubAgent, NeoSwarm as NeoSwarmData } from '@/hooks/api/useChat'
import { ChatAvatar } from '@/components/matrix/neo/neo-message'
import { NeoWorkspace } from '@/components/matrix/neo/neo-workspace'

const EASE = [0.32, 0.72, 0, 1] as const

const DONE_COLOR = 'oklch(0.72 0.14 155)' // the same "ready/success" green the surface uses
const SEGMENTS = 14

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

/** A segmented LED strip that fills as the sub-agent works (and goes fully
 *  green when it finishes / red if it failed). */
function LedBar({ agent }: { agent: NeoSubAgent }) {
  const done = agent.status === 'done'
  const failed = agent.status === 'failed'
  // Running: fill grows with observed steps but never quite completes; settled:
  // every segment lit (green for done, red for failed).
  const lit = done || failed ? SEGMENTS : Math.min(agent.steps.length, SEGMENTS - 1)
  return (
    <span aria-hidden className="flex items-center gap-[2px]">
      {Array.from({ length: SEGMENTS }).map((_, i) => {
        const on = i < lit
        const leading = !done && !failed && on && i === lit - 1
        return (
          <span
            key={i}
            className={cn(
              'h-2.5 w-[3px] rounded-[1px] transition-colors',
              leading && 'animate-pulse',
            )}
            style={{
              background: on ? (failed ? 'oklch(0.62 0.2 25)' : DONE_COLOR) : 'var(--color-muted)',
            }}
          />
        )
      })}
    </span>
  )
}

function StatusGlyph({ status }: { status: NeoSubAgent['status'] }) {
  if (status === 'done') {
    return <Check className="size-3.5 shrink-0" style={{ color: DONE_COLOR }} />
  }
  if (status === 'failed') {
    return <AlertTriangle className="text-destructive size-3.5 shrink-0" />
  }
  return <Loader2 className="text-primary size-3.5 shrink-0 animate-spin" />
}

function AgentRow({
  agent,
  open,
  onToggle,
}: {
  agent: NeoSubAgent
  open: boolean
  onToggle: () => void
}) {
  const line = agent.activity || agent.task || agent.persona || 'working…'
  const hasWindow = agent.steps.length > 0 || !!agent.summary
  return (
    <div className="bg-popover overflow-hidden rounded-2xl">
      <button
        type="button"
        onClick={hasWindow ? onToggle : undefined}
        aria-expanded={open}
        className={cn(
          'flex w-full items-center gap-3 p-3 text-left',
          hasWindow && 'hover:bg-muted/40 transition-colors',
        )}
      >
        <ChatAvatar seed={agent.name} className="size-8" />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="text-foreground truncate text-sm font-semibold">{agent.name}</span>
            <span className="text-muted-foreground/70 ml-auto font-mono text-[0.65rem]">
              {pad2(agent.index)}
            </span>
          </div>
          <div className="mt-0.5 flex items-center gap-2">
            <span className="text-muted-foreground/50 shrink-0 font-mono text-[0.7rem]">└</span>
            <span className="text-muted-foreground line-clamp-1 flex-1 text-xs leading-snug">
              {line}
            </span>
            <LedBar agent={agent} />
          </div>
        </div>
        <StatusGlyph status={agent.status} />
        {hasWindow && (
          <ChevronDown
            className={cn(
              'text-muted-foreground/60 size-4 shrink-0 transition-transform',
              open && 'rotate-180',
            )}
          />
        )}
      </button>
      <AnimatePresence initial={false}>
        {open && hasWindow && (
          <motion.div
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.24, ease: EASE }}
          >
            <div className="space-y-3 px-3 pb-3">
              {agent.steps.length > 0 && <NeoWorkspace steps={agent.steps} />}
              {agent.summary && (
                <p
                  className={cn(
                    'text-xs leading-relaxed whitespace-pre-wrap',
                    agent.status === 'failed' ? 'text-destructive' : 'text-muted-foreground',
                  )}
                >
                  {agent.summary}
                </p>
              )}
            </div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  )
}

/** The completed-agents strip, shown once the swarm settles: each sub-agent's
 *  avatar with its index + a terminal status word. */
function CompletedRow({ agents }: { agents: NeoSubAgent[] }) {
  return (
    <div className="flex flex-wrap gap-2 pt-1">
      {agents.map((a) => (
        <div
          key={a.index}
          className="bg-popover flex items-center gap-2 rounded-xl py-1.5 pr-3 pl-1.5"
          title={a.summary || a.name}
        >
          <span className="relative">
            <ChatAvatar seed={a.name} className="size-7" />
            <span
              className="border-card absolute -right-0.5 -bottom-0.5 grid size-3.5 place-items-center rounded-full border-2"
              style={{ background: a.status === 'failed' ? 'oklch(0.62 0.2 25)' : DONE_COLOR }}
            >
              {a.status === 'failed' ? (
                <AlertTriangle className="size-2 text-white" />
              ) : (
                <Check className="size-2 text-white" />
              )}
            </span>
          </span>
          <span className="leading-tight">
            <span className="text-muted-foreground/70 block font-mono text-[0.6rem]">
              {pad2(a.index)}
            </span>
            <span className="text-foreground block text-[0.7rem] font-medium">
              {a.status === 'failed' ? 'Incomplete' : 'Completed'}
            </span>
          </span>
        </div>
      ))}
    </div>
  )
}

export function NeoSwarm({ swarm }: { swarm: NeoSwarmData }) {
  const [openIndex, setOpenIndex] = useState<number | null>(null)
  if (!swarm || swarm.agents.length === 0) return null
  const count = swarm.count || swarm.agents.length
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <MatrixIcon name="network" className="text-primary size-4 shrink-0" />
        <span className="text-foreground text-sm font-semibold">Agent Swarm</span>
        <span className="text-muted-foreground font-mono text-[0.7rem]">
          · {count} {count === 1 ? 'task' : 'tasks'}
        </span>
        {swarm.done && (
          <span className="text-muted-foreground/70 ml-auto font-mono text-[0.65rem] tracking-wide uppercase">
            done
          </span>
        )}
      </div>
      <div className="flex flex-col gap-2">
        {swarm.agents.map((a) => (
          <AgentRow
            key={a.index}
            agent={a}
            open={openIndex === a.index}
            onToggle={() => setOpenIndex((cur) => (cur === a.index ? null : a.index))}
          />
        ))}
      </div>
      {swarm.done && <CompletedRow agents={swarm.agents} />}
    </div>
  )
}
