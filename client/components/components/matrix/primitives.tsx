'use client'

import { cn } from '@/lib/utils'
import {
  motion,
  useInView,
  useMotionValue,
  useReducedMotion,
  animate as motionAnimate,
} from 'motion/react'
import { useEffect, useRef, useState } from 'react'

/* -------------------------------------------------------------------------- */
/*  ShineButton — adapted from Luxe (guhrodrrigues/luxe) "shine" variant.     */
/* -------------------------------------------------------------------------- */

type ShineButtonProps = React.ComponentProps<'button'> & {
  variant?: 'primary' | 'shine' | 'outline'
}

export function ShineButton({
  className,
  variant = 'shine',
  children,
  ...props
}: ShineButtonProps) {
  if (variant === 'primary') {
    return (
      <button
        {...props}
        className={cn(
          'bg-primary text-primary-foreground inline-flex items-center justify-center gap-2 rounded-xl px-4 py-2 text-sm font-medium',
          'focus-visible:ring-ring focus-visible:ring-offset-background transition-all duration-200 hover:brightness-110 focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none',
          className,
        )}
      >
        {children}
      </button>
    )
  }

  if (variant === 'outline') {
    return (
      <button
        {...props}
        className={cn(
          'border-border bg-card text-foreground inline-flex items-center justify-center gap-2 rounded-xl border px-4 py-2 text-sm font-medium',
          'hover:bg-accent focus-visible:ring-ring transition-colors duration-200 focus-visible:ring-2 focus-visible:outline-none',
          className,
        )}
      >
        {children}
      </button>
    )
  }

  return (
    <button
      {...props}
      className={cn(
        'animate-shine border-border text-foreground/90 inline-flex items-center justify-center gap-2 rounded-xl border bg-[length:200%_100%] px-4 py-2 text-sm font-medium transition-colors',
        'bg-[linear-gradient(110deg,oklch(0.2_0.004_264),45%,oklch(0.32_0.004_264),55%,oklch(0.2_0.004_264))]',
        'focus-visible:ring-ring focus-visible:ring-2 focus-visible:outline-none',
        className,
      )}
    >
      {children}
    </button>
  )
}

/* -------------------------------------------------------------------------- */
/*  AnimatedBorder — adapted from Luxe "animated-border" card wrapper.         */
/* -------------------------------------------------------------------------- */

export function AnimatedBorder({
  children,
  className,
  duration = 6,
}: {
  children: React.ReactNode
  className?: string
  duration?: number
}) {
  const reduce = useReducedMotion()
  return (
    <div className={cn('border-border bg-card relative rounded-2xl border', className)}>
      {!reduce && (
        <div className="pointer-events-none absolute -inset-px overflow-hidden rounded-[inherit] [mask-image:linear-gradient(transparent,transparent),linear-gradient(#000,#000)] [mask-composite:intersect] [mask-clip:padding-box,border-box]">
          <motion.div
            className="via-primary/70 to-primary absolute aspect-square bg-gradient-to-r from-transparent"
            style={{ width: 80, offsetPath: `rect(0 auto auto 0 round 16px)` }}
            animate={{ offsetDistance: ['0%', '100%'] }}
            transition={{ repeat: Number.POSITIVE_INFINITY, duration, ease: 'linear' }}
          />
        </div>
      )}
      <div className="relative h-full rounded-[inherit]">{children}</div>
    </div>
  )
}

/* -------------------------------------------------------------------------- */
/*  Pill / Badge                                                              */
/* -------------------------------------------------------------------------- */

export function Pill({
  children,
  className,
  tone = 'neutral',
}: {
  children: React.ReactNode
  className?: string
  tone?: 'neutral' | 'signal'
}) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-3 py-1 font-mono text-xs',
        tone === 'signal'
          ? 'border-primary/30 bg-primary/10 text-primary'
          : 'border-border bg-card text-muted-foreground',
        className,
      )}
    >
      {children}
    </span>
  )
}

/* -------------------------------------------------------------------------- */
/*  NumberFlow — count-up, inspired by SmoothUI's number-flow.                 */
/* -------------------------------------------------------------------------- */

export function NumberFlow({
  value,
  prefix = '',
  suffix = '',
  className,
}: {
  value: number
  prefix?: string
  suffix?: string
  className?: string
}) {
  const ref = useRef<HTMLSpanElement>(null)
  const inView = useInView(ref, { once: true })
  const reduce = useReducedMotion()
  const mv = useMotionValue(0)
  const [display, setDisplay] = useState(0)

  useEffect(() => {
    if (!inView) return
    if (reduce) {
      setDisplay(value)
      return
    }
    const controls = motionAnimate(mv, value, {
      duration: 1.1,
      ease: [0.16, 1, 0.3, 1],
      onUpdate: (v) => setDisplay(v),
    })
    return () => controls.stop()
  }, [inView, value, reduce, mv])

  return (
    <span ref={ref} className={className}>
      {prefix}
      {Math.round(display)}
      {suffix}
    </span>
  )
}
