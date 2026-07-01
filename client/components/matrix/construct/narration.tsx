'use client'

/**
 * Narration — the agent's language: reasoning, intent, answers.
 *
 * The `role` sub-state changes weight, not shape: `answer` is the product
 * (prominent foreground prose), `thinking` is quiet (muted, a pulsing tick),
 * `intent` reads as a stated objective. Separation is by tone, never stroke.
 */
import type { Narration as NarrationPayload } from '@/lib/construct/types.gen'

export function NarrationView({ narration }: { narration: NarrationPayload }) {
  const role = narration.role || 'answer'
  if (!narration.text) return null

  if (role === 'thinking') {
    return (
      <div className="flex items-start gap-2.5 py-0.5">
        <span className="bg-primary/70 mt-[0.5rem] size-1.5 shrink-0 rounded-full" />
        <p className="text-muted-foreground text-sm leading-relaxed whitespace-pre-wrap">
          {narration.text}
        </p>
      </div>
    )
  }

  if (role === 'intent') {
    return (
      <div className="bg-foreground/[0.03] rounded-xl px-3.5 py-3">
        <p className="text-muted-foreground/70 mb-1 font-mono text-[0.6rem] tracking-[0.14em] uppercase">
          Intent
        </p>
        <p className="text-foreground text-sm leading-relaxed whitespace-pre-wrap">
          {narration.text}
        </p>
      </div>
    )
  }

  return (
    <p className="text-foreground text-[0.95rem] leading-relaxed whitespace-pre-wrap">
      {narration.text}
    </p>
  )
}
