'use client'

/**
 * AgentProfileCard — the premium sub-agent identity card.
 *
 * Inspired by Kimi's subagent character cards, adapted to Centra AI's warm-charcoal
 * design language: tonal elevation (popover on card), sage accent, SquiCircle
 * soft corners, LT Wave typography. Each card carries a procedurally generated
 * geometric avatar, the agent's name + role, a one-line backstory, and a subtle
 * Centra AI wordmark at the bottom.
 *
 * The geometric avatar is rendered as an inline SVG seeded from the agent's name
 * (or an explicit `avatarSeed`), producing a unique but deterministic pattern of
 * concentric shapes in the agent's accent color. No external image assets needed.
 */
import { useMemo } from 'react'
import { motion } from 'motion/react'
import { cn } from '@/lib/utils'
import type { NeoSubAgent, NeoSubAgentProfile } from '@/hooks/api/useChat'
import { pickPersona } from '@/lib/agent-personas'

const EASE = [0.32, 0.72, 0, 1] as const

const STATUS_STYLES = {
  running: { dot: 'bg-pax animate-pulse', label: 'Running' },
  done: { dot: 'bg-[oklch(0.72_0.14_155)]', label: 'Completed' },
  failed: { dot: 'bg-destructive', label: 'Failed' },
} as const

/* ── Geometric Avatar ───────────────────────────────────────────────────── */

function hashSeed(seed: string): number {
  let h = 0
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) | 0
  return Math.abs(h)
}

/** Procedurally generate a geometric pattern SVG from a seed string.
 *  The pattern is a grid of shapes (circles, diamonds, squares) with
 *  varying opacity, unique per seed. Rendered inline for crispness at any size. */
function GeometricAvatar({
  seed,
  color,
  size = 56,
}: {
  seed: string
  color: string
  size?: number
}) {
  const shapes = useMemo(() => {
    const h = hashSeed(seed)
    const grid = 4
    const cell = size / grid
    const items: {
      cx: number
      cy: number
      r: number
      opacity: number
      shape: 'circle' | 'diamond' | 'square'
    }[] = []

    for (let row = 0; row < grid; row++) {
      for (let col = 0; col < grid; col++) {
        const bit = (h >> (row * grid + col)) & 1
        if (!bit) continue
        const cx = col * cell + cell / 2
        const cy = row * cell + cell / 2
        const shapeType = ((h >> (row + col)) & 3) as 0 | 1 | 2 | 3
        const shape = shapeType === 0 ? 'circle' : shapeType === 1 ? 'diamond' : 'square'
        const r = cell * (0.25 + (((h >> (row * 2 + col * 3)) & 7) / 7) * 0.2)
        const opacity = 0.3 + (((h >> (row + col * 5)) & 7) / 7) * 0.7
        items.push({ cx, cy, r, opacity, shape })
      }
    }
    return items
  }, [seed, size])

  const half = size / 2

  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${size} ${size}`}
      className="shrink-0"
      aria-hidden
    >
      {/* Background circle */}
      <circle cx={half} cy={half} r={half} fill={color} opacity={0.12} />
      {/* Outer ring */}
      <circle
        cx={half}
        cy={half}
        r={half - 2}
        fill="none"
        stroke={color}
        strokeWidth={1.5}
        opacity={0.3}
      />
      {/* Inner decorative ring */}
      <circle
        cx={half}
        cy={half}
        r={half * 0.6}
        fill="none"
        stroke={color}
        strokeWidth={1}
        opacity={0.15}
      />
      {/* Procedural shapes */}
      {shapes.map((s, i) => {
        if (s.shape === 'circle') {
          return <circle key={i} cx={s.cx} cy={s.cy} r={s.r} fill={color} opacity={s.opacity} />
        }
        if (s.shape === 'diamond') {
          const d = s.r
          return (
            <polygon
              key={i}
              points={`${s.cx},${s.cy - d} ${s.cx + d},${s.cy} ${s.cx},${s.cy + d} ${s.cx - d},${s.cy}`}
              fill={color}
              opacity={s.opacity}
            />
          )
        }
        // square
        const halfR = s.r * 0.85
        return (
          <rect
            key={i}
            x={s.cx - halfR}
            y={s.cy - halfR}
            width={halfR * 2}
            height={halfR * 2}
            rx={1.5}
            fill={color}
            opacity={s.opacity}
            transform={`rotate(${45 + (hashSeed(seed + String(i)) % 4) * 22.5}, ${s.cx}, ${s.cy})`}
          />
        )
      })}
      {/* Center dot */}
      <circle cx={half} cy={half} r={3} fill={color} opacity={0.9} />
    </svg>
  )
}

/* ── Profile Card ───────────────────────────────────────────────────────── */

export function AgentProfileCard({ agent, className }: { agent: NeoSubAgent; className?: string }) {
  // Resolve profile: use explicit profile, or auto-assign from persona catalog
  const profile: NeoSubAgentProfile = agent.profile ?? {}
  const persona = pickPersona(agent.index, agent.name)
  const role = profile.role ?? persona.role
  const backstory = profile.backstory ?? persona.backstory
  const color = profile.color ?? persona.color
  const avatarSeed = profile.avatarSeed ?? agent.name

  const status = STATUS_STYLES[agent.status]

  return (
    <motion.div
      initial={{ opacity: 0, y: 10 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ duration: 0.3, ease: EASE }}
      className={cn(
        'bg-popover relative overflow-hidden rounded-2xl',
        '[filter:url(#SquiCircleFilter)]',
        className,
      )}
    >
      {/* Accent strip at top */}
      <div className="h-1 w-full rounded-t-2xl" style={{ background: color }} />

      <div className="flex flex-col gap-3 p-4">
        {/* Avatar + Name + Role */}
        <div className="flex items-start gap-3">
          <GeometricAvatar seed={avatarSeed} color={color} size={56} />
          <div className="min-w-0 flex-1">
            <span className="bg-foreground/10 text-foreground inline-block rounded-full px-2.5 py-0.5 text-sm font-semibold">
              {agent.name}
            </span>
            <p className="text-foreground mt-1 text-[0.8rem] leading-tight font-medium">{role}</p>
          </div>
        </div>

        {/* Backstory */}
        {backstory && (
          <p className="text-muted-foreground text-xs leading-relaxed italic">
            &ldquo;{backstory}&rdquo;
          </p>
        )}

        {/* Status + step count */}
        <div className="flex items-center gap-2">
          <span className={cn('size-2 rounded-full', status.dot)} />
          <span className="text-muted-foreground text-xs">
            {status.label}
            {agent.steps.length > 0 && (
              <span className="text-muted-foreground/50">
                {' '}
                &middot; {agent.steps.length} {agent.steps.length === 1 ? 'step' : 'steps'}
              </span>
            )}
          </span>
        </div>
      </div>

      {/* Centra AI wordmark */}
      <div className="border-border/40 border-t px-4 py-2">
        <span className="text-muted-foreground/25 font-mono text-[9px] tracking-[0.2em] uppercase">
          Centra AI
        </span>
      </div>
    </motion.div>
  )
}

/* ── Compact Completed Card ─────────────────────────────────────────────── */

/** A mini profile strip for the completed-agents row. Shows avatar + name +
 *  role in a compact horizontal layout. */
export function AgentProfileMini({ agent, className }: { agent: NeoSubAgent; className?: string }) {
  const profile: NeoSubAgentProfile = agent.profile ?? {}
  const persona = pickPersona(agent.index, agent.name)
  const role = profile.role ?? persona.role
  const color = profile.color ?? persona.color
  const avatarSeed = profile.avatarSeed ?? agent.name

  return (
    <div
      className={cn('bg-popover flex items-center gap-2.5 rounded-xl py-2 pr-3 pl-1.5', className)}
    >
      <span className="relative">
        <GeometricAvatar seed={avatarSeed} color={color} size={28} />
        <span
          className="border-card absolute -right-0.5 -bottom-0.5 grid size-3 place-items-center rounded-full border-2"
          style={{
            background:
              agent.status === 'failed' ? 'var(--color-destructive)' : 'oklch(0.72 0.14 155)',
          }}
        />
      </span>
      <span className="leading-tight">
        <span className="text-foreground block text-[0.7rem] font-medium">{agent.name}</span>
        <span className="text-muted-foreground/70 block text-[0.6rem]">{role}</span>
      </span>
    </div>
  )
}
