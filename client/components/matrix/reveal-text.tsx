'use client'

import { motion, useInView, useReducedMotion } from 'motion/react'
import { useRef } from 'react'

/**
 * RevealText — adapted from SmoothUI (educlopez/smoothui).
 * Reveals its children with a directional slide + fade when scrolled into view.
 */
export interface RevealTextProps {
  children: React.ReactNode
  className?: string
  delay?: number
  direction?: 'up' | 'down' | 'left' | 'right'
  as?: 'span' | 'div' | 'p' | 'h1' | 'h2' | 'h3'
}

const directionVariants = {
  up: { y: 20, opacity: 0 },
  down: { y: -20, opacity: 0 },
  left: { x: 20, opacity: 0 },
  right: { x: -20, opacity: 0 },
}

export function RevealText({
  children,
  className,
  delay = 0,
  direction = 'up',
  as = 'div',
}: RevealTextProps) {
  const ref = useRef<HTMLDivElement>(null)
  const inView = useInView(ref, { once: true, margin: '-10% 0px' })
  const reduce = useReducedMotion()
  const MotionTag = motion[as] as typeof motion.div

  return (
    <MotionTag
      ref={ref as never}
      className={className}
      initial={reduce ? { opacity: 1 } : directionVariants[direction]}
      animate={
        reduce ? { opacity: 1 } : inView ? { x: 0, y: 0, opacity: 1 } : directionVariants[direction]
      }
      transition={reduce ? { duration: 0 } : { duration: 0.4, delay, ease: [0.21, 0.5, 0.51, 1] }}
    >
      {children}
    </MotionTag>
  )
}
