'use client'

/**
 * IdentityScreen — Screen 1 of onboarding. Collects preferred_name,
 * agent_name, and expertise_domains. Writes via PUT /profile once the
 * daemon is active (readiness=active); queues the write if not yet ready.
 * Supports back-nav and partial-progress persistence.
 */
import { useEffect, useState } from 'react'
import { ArrowLeft, ArrowRight, Plus } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@astryxdesign/core/Button'
import { TextInput } from '@astryxdesign/core/TextInput'
import { Token } from '@astryxdesign/core/Token'
import { Heading, Text } from '@astryxdesign/core/Text'
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
        <Heading level={1}>{t('identityTitle')}</Heading>
        <Text type="supporting" color="secondary" display="block">
          {t('identityDesc')}
        </Text>
      </div>

      <div className="mt-8 space-y-5">
        <TextInput
          label={t('preferredName')}
          value={data.preferred_name}
          onChange={(value) => update('preferred_name', value)}
          placeholder={t('preferredNamePlaceholder')}
          size="lg"
          width="100%"
          isRequired
        />

        <TextInput
          label={t('agentName')}
          description={t('agentNameHelp')}
          value={data.agent_name}
          onChange={(value) => update('agent_name', value)}
          placeholder="Neo"
          size="lg"
          width="100%"
        />

        <div className="space-y-2">
          <div className="flex gap-2">
            <TextInput
              label={t('expertise')}
              description={t('expertiseHelp')}
              value={domainInput}
              onChange={setDomainInput}
              onEnter={addDomain}
              placeholder={t('expertisePlaceholder')}
              size="lg"
              width="100%"
              isOptional
            />
            <Button
              label={t('addExpertise')}
              variant="secondary"
              size="lg"
              icon={<Plus className="size-4" />}
              isIconOnly
              onClick={addDomain}
              isDisabled={!domainInput.trim()}
            />
          </div>
          {data.expertise_domains.length > 0 && (
            <div className="flex flex-wrap gap-2 pt-1">
              {data.expertise_domains.map((domain) => (
                <Token
                  key={domain}
                  label={domain}
                  size="sm"
                  onRemove={() => removeDomain(domain)}
                  description={t('removeExpertise')}
                />
              ))}
            </div>
          )}
        </div>
      </div>

      <div className="mt-auto flex items-center gap-3 pt-8">
        <Button label={t('back')} variant="ghost" size="lg" icon={<ArrowLeft />} onClick={onBack} />
        <Button
          label={t('continue')}
          size="lg"
          width="100%"
          isDisabled={submitting || !data.preferred_name.trim()}
          isLoading={submitting}
          endContent={<ArrowRight />}
          onClick={() => void handleSubmit()}
        />
      </div>
    </div>
  )
}
