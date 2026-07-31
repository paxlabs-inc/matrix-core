'use client'

/**
 * NarrationPanel — the `narration` region of the OS shell (frame inversion).
 *
 * In the inverted frame the environment is the page and the conversation with
 * Neo is ONE panel within it (design "Frame inversion", R3.2). This component is
 * that panel: a bounded region container that HOSTS the existing chat thread —
 * it never rebuilds chat, it only places it. The live chat component (NeoSurface,
 * wired to `useChat`) is mounted as this panel's content by the shell wiring.
 *
 * It is deliberately thin: a region wrapper with a stable `data-region` hook the
 * shell uses for layout/tests, separated from the surrounding environment by
 * background tone only (no border strokes — R13.1/R13.2). When the environment
 * stage carries no other surfaces the panel fills the environment (today's
 * chat-centric feel); once Neo's work populates other regions the panel becomes
 * a bounded column beside the stage. Either way it is a region WITHIN the
 * environment root, never the page that contains it.
 */
import type { ReactNode } from 'react'
import { motion } from 'motion/react'
import { cn } from '@/lib/utils'
import { motionTransition } from '@/lib/motion'

export function NarrationPanel({
  children,
  /** When true the panel is a bounded column (the environment stage is showing
   *  other regions beside it); otherwise it fills the environment body. */
  bounded = false,
  className,
}: {
  children: ReactNode
  bounded?: boolean
  className?: string
}) {
  return (
    <motion.section
      layout
      transition={{ layout: motionTransition.layout }}
      data-region="narration"
      aria-label="Conversation with Neo"
      className={cn(
        // bg-background sets this region's tone; the environment stage uses a
        // different tone so the two read as separate layers without any stroke.
        'bg-background relative flex min-h-0 min-w-0 flex-col overflow-hidden',
        // When bounded, chat is a capped column ONLY at lg+ (where the stage sits
        // beside it). Below lg the stage is a full-screen overlay, so chat takes
        // the FULL width — never a cramped half-pane on a phone.
        bounded ? 'w-full lg:max-w-2xl' : 'flex-1',
        className,
      )}
    >
      {children}
    </motion.section>
  )
}
