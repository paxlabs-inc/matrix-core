'use client'

/**
 * The composer docked at the bottom of the market surface.
 *
 * It does NOT hold a conversation. Asking here hands off to the conversation
 * surface — the message is sent there as one ordinary Neo turn on the existing
 * chat seam, and the user lands in the chat with the reply streaming. There is
 * no second chat implementation on this page, and no bespoke finance turn.
 *
 * What it carries across is CONTEXT: the symbol or market the user was reading
 * when they asked, so "why is it down?" arrives as an answerable question.
 */
import { useRouter, useSearchParams } from 'next/navigation'
import { useEffect, useRef, useState } from 'react'
import { useTranslations } from 'next-intl'
import { ChatComposer, ChatComposerInput } from '@astryxdesign/core/Chat'
import { cn } from '@/lib/utils'

/** Where the handoff lands, and what it carries. */
export function askHref(text: string, context?: string): string {
  const params = new URLSearchParams({ ask: text })
  if (context) params.set('ask_context', context)
  return `/?${params.toString()}`
}

/**
 * Consume a market-page handoff once on the conversation route. The ref is the
 * replay guard: rerenders and React's development effect replay cannot submit a
 * second turn before the query parameters are cleared.
 */
export function useHandoffAsk(send: (text: string) => void) {
  const params = useSearchParams()
  const router = useRouter()
  const t = useTranslations('finance')
  const sent = useRef(false)

  useEffect(() => {
    if (sent.current) return
    const ask = params.get('ask')?.trim()
    if (!ask) return
    sent.current = true
    const context = params.get('ask_context')?.trim()
    send(context ? `${ask}\n\n${t('handoffContext', { context })}` : ask)
    router.replace('/')
  }, [params, router, send, t])
}

export function FinanceComposer({
  context,
  placeholder,
  className,
}: {
  /** The market being viewed, e.g. "AAPL — Apple Inc." or "US markets". */
  context?: string
  placeholder: string
  className?: string
}) {
  const router = useRouter()
  const [text, setText] = useState('')

  const submit = (value: string) => {
    const trimmed = value.trim()
    if (!trimmed) return
    setText('')
    // The conversation surface owns the turn and the stream; this only carries
    // the question to it.
    router.push(askHref(trimmed, context))
  }

  return (
    <div className={cn('sticky bottom-0 z-20', className)}>
      <ChatComposer
        value={text}
        onChange={setText}
        onSubmit={submit}
        placeholder={placeholder}
        input={
          <ChatComposerInput
            value={text}
            onChange={setText}
            onSubmit={submit}
            placeholder={placeholder}
            label={placeholder}
          />
        }
        density="compact"
        elevation="low"
      />
    </div>
  )
}
