'use client'

/**
 * IdentityScreen — Screen 1 of onboarding. Collects preferred_name,
 * agent_name, and expertise_domains. Writes via PUT /profile once the
 * daemon is active (readiness=active); queues the write if not yet ready.
 * Supports back-nav and partial-progress persistence.
 */
import { useEffect, useState } from 'react'
import { ArrowLeft, ArrowRight, Loader2, Plus, X } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { toast } from 'sonner'
import { usePutProfile, useProfile } from '@/hooks/api/useOnboarding'
import type { ProfileRequest } from '@/lib/api/onboarding'

interface IdentityData {
  preferred_name: string
  agent_name: string
  expertise_domains: string[]
}

interface Props {
  isActive: boolean
  initialData: IdentityData | null
  onNext: (data: IdentityData) => void
  onBack: () => void
}

export const PENDING_KEY = 'mx-onboarding-identity-pending'

export function loadPending(): IdentityData | null {
  if (typeof window === 'undefined') return null
  try {
    const raw = window.localStorage.getItem(PENDING_KEY)
    return raw ? (JSON.parse(raw) as IdentityData) : null
  } catch {
    return null
  }
}

function savePending(data: IdentityData) {
  if (typeof window === 'undefined') return
  window.localStorage.setItem(PENDING_KEY, JSON.stringify(data))
}

export function clearPending() {
  if (typeof window === 'undefined') return
  window.localStorage.removeItem(PENDING_KEY)
}

export function IdentityScreen({ isActive, initialData, onNext, onBack }: Props) {
  const t = useTranslations('onboarding')
  const putProfile = usePutProfile()
  const profileQuery = useProfile()

  const [data, setData] = useState<IdentityData>(
    initialData ??
      loadPending() ?? {
        preferred_name: '',
        agent_name: 'Neo',
        expertise_domains: [],
      },
  )
  const [domainInput, setDomainInput] = useState('')
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    if (profileQuery.data && !initialData && !loadPending()) {
      setData({
        preferred_name: profileQuery.data.preferred_name ?? '',
        agent_name: profileQuery.data.agent_name ?? 'Neo',
        expertise_domains: profileQuery.data.expertise_domains ?? [],
      })
    }
  }, [profileQuery.data, initialData])

  const addDomain = () => {
    const trimmed = domainInput.trim()
    if (trimmed && !data.expertise_domains.includes(trimmed)) {
      const next = { ...data, expertise_domains: [...data.expertise_domains, trimmed] }
      setData(next)
      savePending(next)
    }
    setDomainInput('')
  }

  const removeDomain = (domain: string) => {
    const next = {
      ...data,
      expertise_domains: data.expertise_domains.filter((d) => d !== domain),
    }
    setData(next)
    savePending(next)
  }

  const update = (field: keyof IdentityData, value: string) => {
    const next = { ...data, [field]: value }
    setData(next)
    savePending(next)
  }

  const handleSubmit = async () => {
    if (!data.preferred_name.trim()) {
      toast.error(t('nameRequired'))
      return
    }
    savePending(data)
    setSubmitting(true)

    if (!isActive) {
      toast.info(t('profileQueued'))
      onNext(data)
      setSubmitting(false)
      return
    }

    try {
      const req: ProfileRequest = {
        preferred_name: data.preferred_name.trim(),
        agent_name: data.agent_name.trim() || 'Neo',
        expertise_domains: data.expertise_domains,
      }
      await putProfile.mutateAsync(req)
      clearPending()
      onNext(data)
    } catch {
      // The daemon is active but the write failed. Stay on the screen and
      // let the user retry — the entered data stays in PENDING_KEY so it is
      // not lost. (On the queued !isActive path above we DO advance, because
      // the onboarding-flow flush re-sends the write once the daemon is up.)
      toast.error(t('profileSaveError'))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="flex flex-1 flex-col">
      <div className="space-y-2">
        <h1 className="text-foreground text-2xl font-semibold tracking-tight">
          {t('identityTitle')}
        </h1>
        <p className="text-muted-foreground text-sm text-pretty">{t('identityDesc')}</p>
      </div>

      <div className="mt-8 space-y-5">
        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('preferredName')}</label>
          <Input
            value={data.preferred_name}
            onChange={(e) => update('preferred_name', e.target.value)}
            placeholder={t('preferredNamePlaceholder')}
            className="h-11"
          />
        </div>

        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('agentName')}</label>
          <Input
            value={data.agent_name}
            onChange={(e) => update('agent_name', e.target.value)}
            placeholder="Neo"
            className="h-11"
          />
          <p className="text-muted-foreground text-xs">{t('agentNameHelp')}</p>
        </div>

        <div className="space-y-2">
          <label className="text-foreground text-sm font-medium">{t('expertise')}</label>
          <div className="flex gap-2">
            <Input
              value={domainInput}
              onChange={(e) => setDomainInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault()
                  addDomain()
                }
              }}
              placeholder={t('expertisePlaceholder')}
              className="h-11"
            />
            <Button
              variant="secondary"
              size="icon"
              onClick={addDomain}
              disabled={!domainInput.trim()}
              aria-label={t('addExpertise')}
            >
              <Plus className="size-4" />
            </Button>
          </div>
          {data.expertise_domains.length > 0 && (
            <div className="flex flex-wrap gap-2 pt-1">
              {data.expertise_domains.map((domain) => (
                <span
                  key={domain}
                  className="bg-primary/10 text-primary flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium"
                >
                  {domain}
                  <button
                    type="button"
                    onClick={() => removeDomain(domain)}
                    className="hover:text-primary/70"
                    aria-label={t('removeExpertise')}
                  >
                    <X className="size-3" />
                  </button>
                </span>
              ))}
            </div>
          )}
          <p className="text-muted-foreground text-xs">{t('expertiseHelp')}</p>
        </div>
      </div>

      <div className="mt-auto flex items-center gap-3 pt-8">
        <Button variant="ghost" size="lg" onClick={onBack}>
          <ArrowLeft data-icon="inline-start" />
          {t('back')}
        </Button>
        <Button
          size="lg"
          className="flex-1"
          disabled={submitting || !data.preferred_name.trim()}
          onClick={() => void handleSubmit()}
        >
          {submitting ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <ArrowRight data-icon="inline-end" />
          )}
          {t('continue')}
        </Button>
      </div>
    </div>
  )
}
