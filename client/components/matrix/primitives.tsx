'use client'

import { Badge } from '@astryxdesign/core/Badge'
import { useInView, useMotionValue, useReducedMotion, animate as motionAnimate } from 'motion/react'
import { useEffect, useRef, useState } from 'react'

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
    <Badge
      label={children}
      variant={tone === 'signal' ? 'info' : 'neutral'}
      className={className}
    />
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
