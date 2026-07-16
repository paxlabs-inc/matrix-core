'use client'

/**
 * InviteGate — the pre-app invite-code entry screen. Calls POST /invite/redeem;
 * on success proceeds to onboarding. Plain-language rejection for
 * invalid/expired/exhausted codes. Never re-asks once redeemed.
 */
import { useState } from 'react'
import { ArrowRight, Loader2 } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { ApiError } from '@/lib/api/client'
import { useRedeemInvite } from '@/hooks/api/useOnboarding'

export function InviteGate({ onRedeemed }: { onRedeemed: () => void }) {
  const t = useTranslations('onboarding')
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const redeem = useRedeemInvite()

  const submit = async () => {
    const trimmed = code.trim()
    if (!trimmed) {
      setError(t('inviteRequired'))
      return
    }
    setError('')
    try {
      await redeem.mutateAsync(trimmed)
      onRedeemed()
    } catch (err) {
      if (err instanceof ApiError) {
        const body = err.body as { error?: string } | undefined
        const msg = body?.error ?? ''
        if (msg.includes('expired')) {
          setError(t('inviteExpired'))
        } else if (msg.includes('available') || msg.includes('exhausted')) {
          setError(t('inviteExhausted'))
        } else if (msg.includes('already redeemed')) {
          onRedeemed()
          return
        } else if (msg.includes('invalid') || err.status === 403) {
          setError(t('inviteInvalid'))
        } else {
          setError(t('inviteError'))
        }
      } else {
        setError(t('inviteError'))
      }
    }
  }

  return (
    <div className="flex flex-1 flex-col justify-center">
      <div className="space-y-2">
        <h1 className="text-foreground text-2xl font-semibold tracking-tight">
          {t('inviteTitle')}
        </h1>
        <p className="text-muted-foreground text-sm text-pretty">{t('inviteDesc')}</p>
      </div>

      <div className="mt-8 space-y-3">
        <Input
          value={code}
          onChange={(e) => setCode(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !redeem.isPending) void submit()
          }}
          placeholder={t('invitePlaceholder')}
          autoFocus
          autoComplete="off"
          spellCheck={false}
          className="h-12 text-base"
        />
        {error && <p className="text-destructive text-sm">{error}</p>}
        <Button
          size="lg"
          className="w-full"
          disabled={redeem.isPending || !code.trim()}
          onClick={() => void submit()}
        >
          {redeem.isPending ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <ArrowRight data-icon="inline-end" />
          )}
          {t('inviteSubmit')}
        </Button>
      </div>
    </div>
  )
}
