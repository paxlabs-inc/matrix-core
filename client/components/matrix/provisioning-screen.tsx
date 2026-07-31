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
import { Dialog } from '@astryxdesign/core/Dialog'
import { Layout } from '@astryxdesign/core/Layout'
import { Center } from '@astryxdesign/core/Center'
import { Spinner } from '@astryxdesign/core/Spinner'
import { Button } from '@astryxdesign/core/Button'
import { Text } from '@astryxdesign/core/Text'
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
    <Dialog
      isOpen
      onOpenChange={() => undefined}
      variant="fullscreen"
      purpose="required"
      padding={0}
    >
      <Layout
        height="fill"
        padding={6}
        className="bg-background"
        content={
          <Center>
            {stage === 'video' ? (
              <div className="flex flex-col items-center gap-4">
                <video
                  className="max-h-[70vh] w-auto max-w-[90vw] rounded-2xl"
                  src="/paxeer_core.mp4"
                  autoPlay
                  muted
                  playsInline
                  onEnded={() => setStage('loading')}
                  onError={() => setStage('loading')}
                />
                <Button label="Skip intro" variant="ghost" onClick={() => setStage('loading')} />
              </div>
            ) : (
              <div className="flex max-w-sm flex-col items-center gap-5 text-center">
                <MatrixLogo size="lg" />
                <Spinner size="xl" label="Setting up your workspace" />
                <Text type="supporting" color="secondary" display="block" aria-live="polite">
                  {STATUS_LINES[line]}
                </Text>
                {slow ? (
                  <Text type="supporting" color="secondary" display="block">
                    First sign-in can take up to a minute while we provision your machine. Hang
                    tight — this only happens once.
                  </Text>
                ) : null}
              </div>
            )}
          </Center>
        }
      />
    </Dialog>
  )
}
