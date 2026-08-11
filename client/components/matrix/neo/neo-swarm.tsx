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
import { CentraIcon, ChevronDown } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { NeoSubAgent, NeoSwarm as NeoSwarmData } from '@/hooks/api/useChat'
import { NeoWorkspace } from '@/components/matrix/neo/neo-workspace'
import { AgentProfileCard, AgentProfileMini } from '@/components/matrix/neo/neo-agent-profile-card'

const EASE = [0.32, 0.72, 0, 1] as const

const DONE_COLOR = 'oklch(0.72 0.14 155)' // the same "ready/success" green the surface uses
const SEGMENTS = 14

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

function AgentRow({
  agent,
  open,
  onToggle,
}: {
  agent: NeoSubAgent
  open: boolean
  onToggle: () => void
}) {
  const hasWindow = agent.steps.length > 0 || !!agent.summary

  return (
    <div className="flex flex-col gap-0">
      {/* Profile card — clickable header that toggles the workspace */}
      <button
        type="button"
        onClick={hasWindow ? onToggle : undefined}
        aria-expanded={open}
        className={cn('w-full text-left', hasWindow && 'cursor-pointer')}
      >
        <AgentProfileCard agent={agent} />
        {/* Expand indicator + LED bar in the card's footer area */}
        {hasWindow && (
          <div className="bg-popover -mt-1 flex items-center gap-2 rounded-b-2xl px-4 pb-3">
            <LedBar agent={agent} />
            <ChevronDown
              className={cn(
                'text-muted-foreground/60 ml-auto size-4 shrink-0 transition-transform',
                open && 'rotate-180',
              )}
            />
          </div>
        )}
      </button>

      {/* Expandable workspace */}
      <AnimatePresence initial={false}>
        {open && hasWindow && (
          <motion.div
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.24, ease: EASE }}
          >
            <div className="space-y-3 pt-2">
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
 *  mini profile card with avatar, name, and role. */
function CompletedRow({ agents }: { agents: NeoSubAgent[] }) {
  return (
    <div className="flex flex-wrap gap-2 pt-1">
      {agents.map((a) => (
        <AgentProfileMini key={a.index} agent={a} />
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
        <CentraIcon name="network" className="text-primary size-4 shrink-0" />
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
