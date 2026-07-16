'use client'

import { useState } from 'react'
import type { FormEvent } from 'react'
import { useTranslations } from 'next-intl'
import { toast } from 'sonner'
import { Bot, CheckCircle, ExternalLink, Lock, RotateCcw, Trash2Icon } from '@/lib/matrix-icons'
import { Button } from '@/components/ui/button'
import {
  useConnectTelegram,
  useDisconnectTelegram,
  useTelegramStatus,
} from '@/hooks/api/useTelegram'

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message.trim() ? error.message : fallback
}

function accountName(username?: string): string {
  const value = username?.trim().replace(/^@/, '')
  return value ? `@${value}` : ''
}

export function TelegramSection() {
  const t = useTranslations('telegram')
  const statusQuery = useTelegramStatus()
  const connect = useConnectTelegram()
  const disconnect = useDisconnectTelegram()
  const [botToken, setBotToken] = useState('')
  const [replacing, setReplacing] = useState(false)

  if (statusQuery.isSuccess && statusQuery.data === null) return null

  const status = statusQuery.data
  const showTokenForm = !status?.configured || replacing
  const pairingURL = status?.pairing_url?.startsWith('https://t.me/')
    ? status.pairing_url
    : undefined
  const requestError = statusQuery.error ?? connect.error ?? disconnect.error

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const token = botToken.trim()
    if (!token) return
    connect.mutate(token, {
      onSuccess: () => {
        setBotToken('')
        setReplacing(false)
        toast.success(t('connectedBot'))
      },
      onError: (error) => toast.error(errorMessage(error, t('connectError'))),
    })
  }

  const remove = () =>
    disconnect.mutate(undefined, {
      onSuccess: () => {
        setBotToken('')
        setReplacing(false)
        toast.success(t('disconnected'))
      },
      onError: (error) => toast.error(errorMessage(error, t('disconnectError'))),
    })

  return (
    <section className="space-y-3">
      <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
        {t('title')}
      </h3>

      <div className="bg-card space-y-4 rounded-lg p-4">
        <div className="flex items-start gap-3">
          <span className="bg-primary/12 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
            <Bot className="size-4.5" />
          </span>
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-medium">{t('heading')}</p>
            <p className="text-muted-foreground text-xs leading-relaxed">{t('description')}</p>
          </div>
        </div>

        {statusQuery.isLoading && (
          <div className="bg-muted text-muted-foreground rounded-lg px-3 py-2.5 text-xs">
            {t('loading')}
          </div>
        )}

        {status && !status.available && (
          <div className="bg-muted rounded-lg px-3 py-2.5">
            <p className="text-foreground text-sm font-medium">{t('unavailable')}</p>
            <p className="text-muted-foreground mt-1 text-xs">
              {status.unavailable_reason || t('unavailableFallback')}
            </p>
          </div>
        )}

        {status?.available && status.configured && (
          <div className="bg-muted space-y-2 rounded-lg p-3">
            <div className="flex items-start gap-2.5">
              <CheckCircle className="text-primary mt-0.5 size-4 shrink-0" />
              <div className="min-w-0 flex-1">
                <p className="text-foreground text-sm font-medium">
                  {accountName(status.bot_username) || t('configuredBot')}
                </p>
                <p className="text-muted-foreground text-xs">
                  {status.paired
                    ? accountName(status.telegram_username) || t('pairedPrivateChat')
                    : status.connected
                      ? t('waitingForPairing')
                      : t('reconnecting')}
                </p>
              </div>
            </div>

            {!status.paired && pairingURL && (
              <a
                href={pairingURL}
                target="_blank"
                rel="noopener noreferrer"
                className="bg-primary text-primary-foreground hover:bg-primary/90 flex w-full items-center justify-center gap-2 rounded-lg px-3 py-2.5 text-sm font-semibold transition-colors"
              >
                {t('openPairing')}
                <ExternalLink className="size-4" />
              </a>
            )}

            {!status.paired && <p className="text-muted-foreground text-xs">{t('pairingHelp')}</p>}
          </div>
        )}

        {status?.last_error && (
          <p className="bg-destructive/10 text-destructive rounded-lg px-3 py-2.5 text-xs">
            {status.last_error}
          </p>
        )}

        {requestError && (
          <p className="bg-destructive/10 text-destructive rounded-lg px-3 py-2.5 text-xs">
            {errorMessage(requestError, t('statusError'))}
          </p>
        )}

        {status?.available && showTokenForm && (
          <form className="space-y-3" onSubmit={submit}>
            <div>
              <label htmlFor="telegram-bot-token" className="text-foreground text-sm font-medium">
                {replacing ? t('replacementToken') : t('tokenLabel')}
              </label>
              <input
                id="telegram-bot-token"
                name="telegram-bot-token"
                type="password"
                autoComplete="off"
                spellCheck={false}
                value={botToken}
                onChange={(event) => {
                  setBotToken(event.target.value)
                  connect.reset()
                }}
                placeholder={t('tokenPlaceholder')}
                className="bg-background text-foreground placeholder:text-muted-foreground/60 focus:ring-primary mt-1.5 w-full rounded-lg px-3 py-2.5 font-mono text-sm outline-none focus:ring-2"
              />
            </div>
            <p className="text-muted-foreground text-xs leading-relaxed">
              {t('botFatherHelp')}{' '}
              <a
                href="https://t.me/BotFather"
                target="_blank"
                rel="noopener noreferrer"
                className="text-primary inline-flex items-center gap-1 font-medium hover:underline"
              >
                {t('openBotFather')}
                <ExternalLink className="size-3" />
              </a>
            </p>
            <div className="flex flex-wrap gap-2">
              <Button type="submit" size="sm" disabled={!botToken.trim() || connect.isPending}>
                <Bot className="size-4" />
                {replacing ? t('replaceAction') : t('connectAction')}
              </Button>
              {replacing && (
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    setBotToken('')
                    setReplacing(false)
                    connect.reset()
                  }}
                >
                  {t('cancel')}
                </Button>
              )}
            </div>
          </form>
        )}

        {status?.available && status.configured && !replacing && (
          <div className="flex flex-wrap gap-2">
            <Button variant="secondary" size="sm" onClick={() => setReplacing(true)}>
              <RotateCcw className="size-4" />
              {t('replace')}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              className="text-destructive hover:text-destructive"
              onClick={remove}
              disabled={disconnect.isPending}
            >
              <Trash2Icon className="size-4" />
              {t('disconnect')}
            </Button>
          </div>
        )}

        {status?.available && (
          <p className="text-muted-foreground flex items-start gap-1.5 text-xs leading-relaxed">
            <Lock className="text-primary mt-0.5 size-3.5 shrink-0" />
            {t('secureStorage')}
          </p>
        )}
      </div>
    </section>
  )
}
