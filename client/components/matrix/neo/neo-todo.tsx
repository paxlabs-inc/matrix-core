'use client'

/**
 * NeoTodoList — the live task-list checklist (neo-smoothness req.3).
 *
 * Surfaced from `tool.todo` events: Neo lays out a short ordered plan and ticks
 * it off in real time, so on a multi-step task the user always knows what is
 * done, in progress, and pending. The full list is REPLACED on every update
 * (the agent sends the complete current list each time), so this always renders
 * the latest plan. The state rides the durable trace (buildTaskFromTrace), so it
 * survives conversation reopen + agent respawn (req.3.5).
 *
 * Design system: separation by background TONE only (bg-card / bg-muted), no
 * border strokes for depth, single accent via text-primary, the surface's
 * "ready/success" green for done, no emojis/glow.
 */
import { motion } from 'motion/react'
import { Check, CircleIcon, Loader2 } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { NeoTodoItem } from '@/hooks/api/useChat'

const DONE_COLOR = 'oklch(0.72 0.14 155)' // the surface's "ready/success" green

function TodoRow({ item }: { item: NeoTodoItem }) {
  const done = item.status === 'done'
  const active = item.status === 'in_progress'
  return (
    <motion.li
      layout
      initial={{ opacity: 0, y: 4 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.2 }}
      className={cn(
        'flex items-start gap-2.5 rounded-lg px-2.5 py-2 transition-colors',
        active ? 'bg-muted' : 'bg-transparent',
      )}
    >
      <span className="mt-0.5 grid size-4 shrink-0 place-items-center">
        {done ? (
          <Check className="size-4" style={{ color: DONE_COLOR }} />
        ) : active ? (
          <Loader2 className="text-primary size-4 animate-spin" />
        ) : (
          <CircleIcon className="text-muted-foreground/50 size-3.5" />
        )}
      </span>
      <span
        className={cn(
          'text-[0.82rem] leading-snug',
          done && 'text-muted-foreground line-through',
          active && 'text-foreground font-medium',
          !done && !active && 'text-muted-foreground',
        )}
      >
        {item.text}
      </span>
    </motion.li>
  )
}

/** Render the live task-list checklist (from tool.todo events). */
export function NeoTodoList({ todos }: { todos?: NeoTodoItem[] }) {
  if (!todos || todos.length === 0) return null
  const done = todos.filter((t) => t.status === 'done').length
  return (
    <div className="bg-card flex flex-col gap-2 rounded-xl p-3">
      <div className="flex items-center justify-between px-1.5">
        <p className="text-foreground text-sm font-bold">Plan</p>
        <p className="text-muted-foreground font-mono text-[0.7rem]">
          {done}/{todos.length} done
        </p>
      </div>
      <ul className="flex flex-col gap-0.5">
        {todos.map((item, i) => (
          <TodoRow key={`${i}-${item.text}`} item={item} />
        ))}
      </ul>
    </div>
  )
}
