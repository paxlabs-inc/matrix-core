'use client'

/**
 * OnboardingFlow — the guided three-screen onboarding that replaces the
 * old blank loading wait. The flow is:
 *
 *   invite gate → Screen 1 Identity → Screen 2 Disclosure+Consent → Screen 3 Wallet → done
 *
 * Provisioning is triggered at invite redemption (server-side) and polled
 * throughout via useProvisionStatus. Screens 1-2 hit only the central router
 * and proceed during provisioning. The profile write (Screen 1) and the
 * wallet step (Screen 3) gate on readiness=active.
 *
 * Partial progress is persisted to localStorage so a refresh resumes at the
 * furthest-reached screen. Once complete, the `mx-onboarded` flag is set
 * and the flow is never shown again.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { useAuth } from '@/lib/auth/AuthProvider'
import { useProvisionStatus, usePutProfile } from '@/hooks/api/useOnboarding'
import { InviteGate } from '@/components/matrix/onboarding/invite-gate'
import {
  IdentityScreen,
  clearPending as clearPendingProfile,
  loadPending as loadPendingProfile,
} from '@/components/matrix/onboarding/identity-screen'
import { DisclosureScreen } from '@/components/matrix/onboarding/disclosure-screen'
import { WalletScreen } from '@/components/matrix/onboarding/wallet-screen'
import { MatrixLogo } from '@/components/matrix/matrix-logo'
import { cn } from '@/lib/utils'

const STORAGE_KEY = 'mx-onboarding-step'
const ONBOARDED_KEY = 'mx-onboarded'

export type OnboardingStep = 'invite' | 'identity' | 'disclosure' | 'wallet' | 'done'

const STEP_INDEX: Record<OnboardingStep, number> = {
  invite: 0,
  identity: 1,
  disclosure: 2,
  wallet: 3,
  done: 4,
}

function loadStep(): OnboardingStep {
  if (typeof window === 'undefined') return 'invite'
  if (window.localStorage.getItem(ONBOARDED_KEY) === '1') return 'done'
  const saved = window.localStorage.getItem(STORAGE_KEY)
  if (saved && STEP_INDEX[saved as OnboardingStep] !== undefined) {
    return saved as OnboardingStep
  }
  return 'invite'
}

function saveStep(step: OnboardingStep) {
  if (typeof window === 'undefined') return
  if (step === 'done') {
    window.localStorage.setItem(ONBOARDED_KEY, '1')
    window.localStorage.removeItem(STORAGE_KEY)
  } else {
    window.localStorage.setItem(STORAGE_KEY, step)
  }
}

export function OnboardingFlow({ onComplete }: { onComplete: () => void }) {
  const { user, enabled: authEnabled } = useAuth()
  const [step, setStep] = useState<OnboardingStep>(loadStep)
  const [profileData, setProfileData] = useState<{
    preferred_name: string
    agent_name: string
    expertise_domains: string[]
  } | null>(null)

  const provision = useProvisionStatus(authEnabled && !!user && step !== 'done')
  const putProfile = usePutProfile()
  const flushedRef = useRef(false)

  const readiness = provision.data?.state ?? 'none'
  const isActive = readiness === 'active'
  const isFailed = readiness === 'failed'

  // Flush the queued profile write once the daemon is reachable. The
  // Identity screen persists the entered profile to localStorage and
  // proceeds during provisioning (the daemon isn't active yet on the
  // fast path); without this, the PUT /profile would never be sent and
  // Neo's identity would never be seeded (req 2.1/2.4, 4.2). Idempotent:
  // a successful flush clears the pending record, and flushedRef guards
  // against re-entry while the mutation is in flight.
  useEffect(() => {
    if (!isActive || flushedRef.current) return
    const pending = loadPendingProfile()
    if (!pending || !pending.preferred_name?.trim()) return
    flushedRef.current = true
    putProfile.mutate(
      {
        preferred_name: pending.preferred_name.trim(),
        agent_name: pending.agent_name?.trim() || 'Neo',
        expertise_domains: pending.expertise_domains ?? [],
      },
      {
        onSuccess: () => clearPendingProfile(),
        // Allow a later attempt (e.g. a transient daemon error) to retry.
        onError: () => {
          flushedRef.current = false
        },
      },
    )
  }, [isActive, putProfile])

  const goTo = useCallback((next: OnboardingStep) => {
    setStep(next)
    saveStep(next)
  }, [])

  const handleComplete = useCallback(() => {
    saveStep('done')
    setStep('done')
    onComplete()
  }, [onComplete])

  useEffect(() => {
    if (step === 'done') {
      onComplete()
    }
  }, [step, onComplete])

  if (!authEnabled || !user) {
    onComplete()
    return null
  }

  if (step === 'done') return null

  const currentIdx = STEP_INDEX[step]
  const totalSteps = 3

  return (
    <main className="bg-background fixed inset-0 z-50 flex flex-col items-center overflow-y-auto">
      <div className="flex w-full max-w-md flex-1 flex-col px-6 py-8">
        <div className="mb-8 flex items-center justify-between">
          <MatrixLogo size="sm" />
          {currentIdx > 0 && currentIdx <= totalSteps && (
            <div className="flex items-center gap-1.5">
              {Array.from({ length: totalSteps }, (_, i) => (
                <span
                  key={i}
                  className={cn(
                    'h-1.5 w-6 rounded-full transition-colors',
                    i < currentIdx && 'bg-primary',
                    i === currentIdx - 1 && 'bg-primary',
                    i > currentIdx - 1 && 'bg-muted',
                  )}
                />
              ))}
            </div>
          )}
        </div>

        <div className="flex flex-1 flex-col">
          {step === 'invite' && <InviteGate onRedeemed={() => goTo('identity')} />}

          {step === 'identity' && (
            <IdentityScreen
              isActive={isActive}
              initialData={profileData}
              onNext={(data) => {
                setProfileData(data)
                goTo('disclosure')
              }}
              onBack={() => goTo('invite')}
            />
          )}

          {step === 'disclosure' && (
            <DisclosureScreen onNext={() => goTo('wallet')} onBack={() => goTo('identity')} />
          )}

          {step === 'wallet' && (
            <WalletScreen
              isActive={isActive}
              onNext={() => handleComplete()}
              onBack={() => goTo('disclosure')}
            />
          )}
        </div>

        {step !== 'invite' && (
          <div className="mt-6 flex items-center justify-center gap-2 text-xs">
            <ReadinessIndicator state={readiness} />
          </div>
        )}

        {isFailed && step !== 'invite' && (
          <div className="bg-destructive/10 mt-4 rounded-lg p-3 text-center">
            <p className="text-destructive text-xs">
              Your agent runtime couldn&apos;t start. You can continue and retry from Settings.
            </p>
          </div>
        )}
      </div>
    </main>
  )
}

function ReadinessIndicator({ state }: { state: string }) {
  const label =
    state === 'active'
      ? 'Agent ready'
      : state === 'provisioning'
        ? 'Setting up your agent…'
        : state === 'failed'
          ? 'Setup issue'
          : ''

  if (!label) return null

  return (
    <span className="text-muted-foreground flex items-center gap-1.5">
      <span
        className={cn(
          'size-1.5 rounded-full',
          state === 'active' && 'bg-primary',
          state === 'provisioning' && 'bg-primary/60 animate-pulse',
          state === 'failed' && 'bg-destructive',
        )}
      />
      {label}
    </span>
  )
}

export { ONBOARDED_KEY, STORAGE_KEY }
