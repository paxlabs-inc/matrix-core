'use client'

/**
 * WalletApproval — the consent surface for a money/approval gate.
 *
 * This IS the entire trust surface: per the north star, the user's only
 * authority over irreversible actions is CONSENT, so the gate must be
 * unbypassable and legible. Neo holds no signing key (neo.frozen.kvx i1), so
 * "I can't sign on my own" is literally true. The slide-to-sign gesture makes
 * approval deliberate; a swipe can't be a misclick.
 *
 * Wired to the in-walk MCL gate: approve → answerGate(true), deny →
 * answerGate(false). The gate question carries the human-legible details
 * (amount / recipient), so we surface it verbatim rather than inventing a
 * structured tx card we don't have.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { animate, motion, useMotionValue } from 'motion/react'
import { ArrowRight, Check, ShieldCheck } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { PendingGate } from '@/hooks/api/useChat'

const KNOB = 48
const PAD = 5
const COMMIT = 0.82 // fraction of the track the knob must cross to commit

// Success green — a semantic confirm color (not a theme token). Kept as inline
// style so it survives Tailwind's arbitrary-value parsing for oklch + alpha.
const OK = 'oklch(0.72 0.14 155)'
const OK_SOFT = 'oklch(0.72 0.14 155 / 0.15)'
const OK_TEXT = 'oklch(0.82 0.13 155)'

function SlideToSign({ label, onComplete }: { label: string; onComplete: () => void }) {
  const trackRef = useRef<HTMLDivElement>(null)
  const x = useMotionValue(0)
  const [maxX, setMaxX] = useState(0)
  const [signed, setSigned] = useState(false)

  useEffect(() => {
    const measure = () => {
      const w = trackRef.current?.clientWidth ?? 0
      setMaxX(Math.max(0, w - KNOB - PAD * 2))
    }
    measure()
    window.addEventListener('resize', measure)
    return () => window.removeEventListener('resize', measure)
  }, [])

  const handleDragEnd = useCallback(() => {
    if (signed) return
    if (maxX > 0 && x.get() > maxX * COMMIT) {
      setSigned(true)
      animate(x, maxX, { type: 'spring', stiffness: 400, damping: 38 })
      onComplete()
    } else {
      animate(x, 0, { type: 'spring', stiffness: 500, damping: 40 })
    }
  }, [signed, maxX, x, onComplete])

  return (
    <div
      ref={trackRef}
      className={cn(
        'relative h-[3.25rem] overflow-hidden rounded-full transition-colors select-none',
        !signed && 'bg-muted',
      )}
      style={signed ? { background: OK_SOFT } : undefined}
    >
      <div className="pointer-events-none absolute inset-0 grid place-items-center pl-7 text-center text-sm font-semibold">
        <span
          className={signed ? undefined : 'text-muted-foreground'}
          style={signed ? { color: OK_TEXT } : undefined}
        >
          {signed ? 'Approved — broadcasting…' : label}
        </span>
      </div>
      <motion.button
        type="button"
        drag={signed ? false : 'x'}
        dragConstraints={{ left: 0, right: maxX }}
        dragElastic={0}
        dragMomentum={false}
        style={{ x, ...(signed ? { background: OK, color: '#04140e' } : undefined) }}
        onDragEnd={handleDragEnd}
        aria-label={label}
        className={cn(
          'absolute top-[5px] left-[5px] grid size-12 cursor-grab touch-none place-items-center rounded-full transition-colors active:cursor-grabbing',
          !signed && 'bg-primary text-white',
        )}
      >
        {signed ? <Check className="size-5" /> : <ArrowRight className="size-5" />}
      </motion.button>
    </div>
  )
}

export function WalletApproval({
  gate,
  onApprove,
  onDeny,
}: {
  gate: PendingGate
  onApprove: (answer?: string) => void
  onDeny: () => void
}) {
  const approveOpt = gate.options.find((o) => /approve|confirm|yes|sign|send|pay/i.test(o))
  const denyOpt = gate.options.find((o) => /deny|cancel|no|reject|stop/i.test(o))

  return (
    <div className="flex flex-col gap-4">
      <div className="bg-primary/10 flex gap-3 rounded-2xl px-4 py-3">
        <ShieldCheck className="text-primary mt-0.5 size-4 shrink-0" />
        <p className="text-sm leading-relaxed">
          This moves real funds, so I can&rsquo;t sign on my own. The one step that needs you is the
          approval below.
        </p>
      </div>

      <div className="bg-muted rounded-2xl p-4">
        <p className="text-foreground text-[0.95rem] leading-relaxed">{gate.question}</p>
      </div>

      <SlideToSign
        label={approveOpt ? `Slide to ${approveOpt.toLowerCase()}` : 'Slide to approve'}
        onComplete={() => onApprove(approveOpt)}
      />

      <button
        type="button"
        onClick={() => onDeny()}
        className="text-muted-foreground hover:text-foreground mx-auto text-xs transition-colors"
      >
        {denyOpt ? `${denyOpt} — don't do this` : 'Not now — cancel this'}
      </button>

      <p className="text-muted-foreground/80 text-center text-[0.7rem]">
        Your wallet signs locally. Neo never holds your key.
      </p>
    </div>
  )
}
