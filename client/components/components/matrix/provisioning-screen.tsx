'use client'

/**
 * ProvisioningScreen — the full-screen "setting up" experience shown to a
 * brand-new user while the router provisions their private daemon machine
 * (≈1 min on first sign-in). It replaces the old behaviour where the
 * dashboard rendered static mock fixtures during that window.
 *
 * Two steps:
 *   1. Intro video (public/paxeer_core.mp4) — first sign-in only.
 *   2. Box-loader + cycling status lines until the machine answers.
 *
 * The gate (provisioning-gate.tsx) owns readiness; this is presentation.
 */
import { useEffect, useState } from 'react'
import Loader from '@/components/ui/box-loader'
import { MatrixLogo } from '@/components/matrix/matrix-logo'

const STATUS_LINES = [
  'Spinning up your private agent runtime…',
  'Provisioning secure, isolated compute…',
  'Loading your skills and tools…',
  'Connecting to the Paxeer network…',
  'Putting the finishing touches…',
]

export function ProvisioningScreen({ firstTime }: { firstTime: boolean }) {
  const [stage, setStage] = useState<'video' | 'loading'>(firstTime ? 'video' : 'loading')
  const [line, setLine] = useState(0)
  const [slow, setSlow] = useState(false)

  // Cycle the reassuring status copy while loading.
  useEffect(() => {
    if (stage !== 'loading') return
    const id = setInterval(() => setLine((i) => (i + 1) % STATUS_LINES.length), 2600)
    return () => clearInterval(id)
  }, [stage])

  // After a while, acknowledge the wait so it doesn't feel stuck.
  useEffect(() => {
    const id = setTimeout(() => setSlow(true), 75_000)
    return () => clearTimeout(id)
  }, [])

  return (
    <main className="bg-background fixed inset-0 z-50 flex flex-col items-center justify-center gap-8 px-6">
      {stage === 'video' ? (
        <>
          <video
            className="max-h-[70vh] w-auto max-w-[90vw] rounded-2xl"
            src="/paxeer_core.mp4"
            autoPlay
            muted
            playsInline
            onEnded={() => setStage('loading')}
            onError={() => setStage('loading')}
          />
          <button
            type="button"
            onClick={() => setStage('loading')}
            className="text-muted-foreground hover:text-foreground text-xs underline-offset-4 hover:underline"
          >
            Skip intro
          </button>
        </>
      ) : (
        <>
          <MatrixLogo size="lg" />
          <Loader size="lg" />
          {/* The lg box-loader's 3D animation translates its cubes well below
              the stage's footprint, so the status copy is pushed further down
              (beyond the flex gap) to clear the rolling boxes. */}
          <div className="mt-12 flex max-w-sm flex-col items-center gap-1 text-center">
            <p className="text-foreground text-sm font-medium">Setting up your workspace</p>
            <p className="text-muted-foreground text-xs text-pretty" aria-live="polite">
              {STATUS_LINES[line]}
            </p>
            {slow && (
              <p className="text-muted-foreground/70 mt-2 text-[11px] text-pretty">
                First sign-in can take up to a minute while we provision your machine. Hang tight —
                this only happens once.
              </p>
            )}
          </div>
        </>
      )}
    </main>
  )
}
