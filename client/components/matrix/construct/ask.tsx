'use client'

/**
 * Ask — the blocking, typed request-for-human; kind in
 * { choose | input | confirm | sign | upload }. The ONE inherently
 * bidirectional primitive: the renderer collects a typed response and hands it
 * back via `onRespond` (the daemon round-trip is wired in Phase 5). Once a
 * response is attached, it renders its settled state instead of the control.
 *
 * `sign` carries the heaviest stakes (a wallet signature / irreversible act),
 * so it adopts the destructive tone and a deliberate confirm; the real wallet
 * path is reused at Phase 5.
 */
import { useState } from 'react'
import { Check, ShieldCheck, ArrowUpFromLine } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { Ask as AskPayload, AskResponse } from '@/lib/construct/types.gen'

export function AskView({
  id,
  ask,
  onRespond,
}: {
  id: string
  ask: AskPayload
  onRespond?: (surfaceId: string, response: AskResponse) => void
}) {
  const [value, setValue] = useState('')
  const answered = !!ask.response
  const respond = (r: AskResponse) =>
    onRespond?.(id, { ...r, answered_at: new Date().toISOString() })

  return (
    <div
      className={cn(
        'rounded-2xl px-4 py-3.5',
        ask.ask_kind === 'sign' ? 'bg-destructive/[0.07]' : 'bg-primary/[0.06]',
      )}
    >
      <p className="text-foreground text-sm leading-relaxed font-medium">{ask.prompt}</p>

      {answered ? (
        <AnsweredState ask={ask} />
      ) : (
        <div className="mt-3">
          {ask.ask_kind === 'choose' && (
            <div className="flex flex-wrap gap-2">
              {(ask.options ?? []).map((o) => (
                <button
                  key={o.id}
                  type="button"
                  onClick={() => respond({ choice: o.id })}
                  className="bg-foreground/[0.06] hover:bg-primary hover:text-primary-foreground text-foreground/90 rounded-full px-3.5 py-1.5 text-sm font-medium transition-colors"
                >
                  {o.label}
                </button>
              ))}
            </div>
          )}

          {ask.ask_kind === 'input' && (
            <form
              className="flex gap-2"
              onSubmit={(e) => {
                e.preventDefault()
                if (value.trim()) respond({ value: value.trim() })
              }}
            >
              <input
                value={value}
                onChange={(e) => setValue(e.target.value)}
                placeholder={ask.expected || 'Your answer'}
                className="bg-background text-foreground placeholder:text-muted-foreground/60 focus:ring-primary min-w-0 flex-1 rounded-full px-3.5 py-2 text-sm outline-none focus:ring-2"
              />
              <button
                type="submit"
                disabled={!value.trim()}
                className="bg-primary text-primary-foreground hover:bg-primary/90 rounded-full px-4 py-2 text-sm font-semibold transition-colors disabled:opacity-40"
              >
                Send
              </button>
            </form>
          )}

          {ask.ask_kind === 'confirm' && (
            <div className="flex gap-2">
              <button
                type="button"
                onClick={() => respond({ confirmed: true })}
                className="bg-primary text-primary-foreground hover:bg-primary/90 rounded-full px-4 py-2 text-sm font-semibold transition-colors"
              >
                Confirm
              </button>
              <button
                type="button"
                onClick={() => respond({ confirmed: false })}
                className="bg-foreground/[0.06] hover:bg-foreground/[0.1] text-foreground/85 rounded-full px-4 py-2 text-sm font-medium transition-colors"
              >
                Decline
              </button>
            </div>
          )}

          {ask.ask_kind === 'sign' && (
            <button
              type="button"
              onClick={() => respond({ confirmed: true })}
              className="bg-destructive text-primary-foreground hover:bg-destructive/90 inline-flex items-center gap-2 rounded-full px-4 py-2 text-sm font-semibold transition-colors"
            >
              <ShieldCheck className="size-4" />
              Review &amp; sign
            </button>
          )}

          {ask.ask_kind === 'upload' && (
            <label className="bg-foreground/[0.06] hover:bg-foreground/[0.1] text-foreground/85 inline-flex cursor-pointer items-center gap-2 rounded-full px-4 py-2 text-sm font-medium transition-colors">
              <ArrowUpFromLine className="size-4" />
              Choose file
              <input
                type="file"
                className="hidden"
                onChange={(e) => {
                  const f = e.target.files?.[0]
                  if (f) respond({ upload_ref: f.name })
                }}
              />
            </label>
          )}
        </div>
      )}
    </div>
  )
}

function AnsweredState({ ask }: { ask: AskPayload }) {
  const r = ask.response
  if (!r) return null
  let summary = 'Answered'
  if (ask.ask_kind === 'choose') {
    const opt = (ask.options ?? []).find((o) => o.id === r.choice)
    summary = `Chose ${opt?.label || r.choice || ''}`
  } else if (ask.ask_kind === 'input') {
    summary = r.value || 'Answered'
  } else if (ask.ask_kind === 'confirm') {
    summary = r.confirmed ? 'Confirmed' : 'Declined'
  } else if (ask.ask_kind === 'sign') {
    summary = 'Signed'
  } else if (ask.ask_kind === 'upload') {
    summary = `Uploaded ${r.upload_ref || ''}`
  }
  return (
    <p className="text-muted-foreground mt-2.5 inline-flex items-center gap-1.5 text-sm">
      <Check className="text-primary size-4" />
      {summary}
    </p>
  )
}
