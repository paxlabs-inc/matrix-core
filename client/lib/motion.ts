import type { Transition } from 'motion/react'

export const motionEase = [0.32, 0.72, 0, 1] as const

export const motionTransition = {
  quick: { duration: 0.18, ease: motionEase } satisfies Transition,
  content: { duration: 0.24, ease: motionEase } satisfies Transition,
  page: { duration: 0.32, ease: motionEase } satisfies Transition,
  layout: {
    type: 'spring',
    stiffness: 360,
    damping: 38,
    mass: 0.9,
  } satisfies Transition,
} as const

export const pageMotion = {
  initial: { opacity: 0, y: 8 },
  animate: { opacity: 1, y: 0 },
  exit: { opacity: 0, y: -4 },
} as const

export const contentMotion = {
  initial: { opacity: 0, y: 6 },
  animate: { opacity: 1, y: 0 },
  exit: { opacity: 0, y: -4 },
} as const
