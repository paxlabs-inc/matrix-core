'use client'

/**
 * Entity — a referenceable typed object: type + identity + fields + affordances.
 *
 * This is the surface that gives world-state an IDENTITY (a tx, token, file,
 * sub-agent, API result-as-object) so it can be re-referenced and acted on.
 * Fields are key/value rows (a `ref` field links to another surface);
 * affordances are the trusted actions the agent attached — open a link, copy
 * an identity, or fire an Ask (the bridge to the back-channel).
 */
import { useCallback, useState } from 'react'
import { Check, Copy, ExternalLink, Package } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import type { Entity as EntityPayload, Affordance } from '@/lib/construct/types.gen'

function useCopy(): [boolean, (text: string) => void] {
  const [copied, setCopied] = useState(false)
  const copy = useCallback((text: string) => {
    if (typeof navigator === 'undefined' || !navigator.clipboard) return
    navigator.clipboard.writeText(text).then(
      () => {
        setCopied(true)
        setTimeout(() => setCopied(false), 1200)
      },
      () => {},
    )
  }, [])
  return [copied, copy]
}

function looksLikeId(v: string): boolean {
  return /^(0x[0-9a-fA-F]{6,}|[A-Za-z0-9]{20,})$/.test(v.trim())
}

function AffordanceButton({
  affordance,
  identity,
  onAsk,
}: {
  affordance: Affordance
  identity: string
  onAsk?: (askRef: string) => void
}) {
  const [copied, copy] = useCopy()
  const kind = affordance.kind || (affordance.href ? 'link' : 'copy')

  if (kind === 'link' && affordance.href) {
    return (
      <a
        href={affordance.href}
        target="_blank"
        rel="noreferrer noopener"
        className="bg-foreground/[0.06] hover:bg-foreground/[0.1] text-foreground/85 inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium transition-colors"
      >
        <ExternalLink className="size-3" />
        {affordance.label}
      </a>
    )
  }
  if (kind === 'ask') {
    return (
      <button
        type="button"
        onClick={() => onAsk?.(affordance.ask_ref || affordance.id)}
        className="bg-primary text-primary-foreground hover:bg-primary/90 inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-semibold transition-colors"
      >
        {affordance.label}
      </button>
    )
  }
  return (
    <button
      type="button"
      onClick={() => copy(affordance.href || identity)}
      className="bg-foreground/[0.06] hover:bg-foreground/[0.1] text-foreground/85 inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium transition-colors"
    >
      {copied ? <Check className="text-primary size-3" /> : <Copy className="size-3" />}
      {affordance.label}
    </button>
  )
}

export function EntityView({
  entity,
  irreversible,
  onAsk,
}: {
  entity: EntityPayload
  irreversible?: boolean
  onAsk?: (askRef: string) => void
}) {
  const [copied, copy] = useCopy()
  return (
    <div
      className={cn(
        'rounded-2xl px-4 py-3.5',
        irreversible ? 'bg-destructive/[0.07]' : 'bg-foreground/[0.03]',
      )}
    >
      <div className="flex items-center gap-2.5">
        <span
          className={cn(
            'grid size-8 shrink-0 place-items-center rounded-[0.7rem]',
            irreversible ? 'bg-destructive/15 text-destructive' : 'bg-primary/15 text-primary',
          )}
        >
          <Package className="size-[1.05rem]" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-muted-foreground/70 font-mono text-[0.6rem] tracking-[0.12em] uppercase">
            {entity.type}
          </p>
          <p className="text-foreground truncate text-sm font-semibold">
            {entity.label || entity.identity}
          </p>
        </div>
        {looksLikeId(entity.identity) && (
          <button
            type="button"
            onClick={() => copy(entity.identity)}
            title="Copy identity"
            className="text-muted-foreground hover:bg-foreground/[0.06] hover:text-foreground grid size-7 shrink-0 place-items-center rounded-full transition"
          >
            {copied ? <Check className="text-primary size-3.5" /> : <Copy className="size-3.5" />}
          </button>
        )}
      </div>

      {entity.label && entity.identity !== entity.label && (
        <p className="text-muted-foreground/80 mt-1.5 truncate font-mono text-[0.72rem]">
          {entity.identity}
        </p>
      )}

      {entity.fields && entity.fields.length > 0 && (
        <dl className="mt-3 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5">
          {entity.fields.map((f, i) => (
            <div key={`${f.key}-${i}`} className="contents">
              <dt className="text-muted-foreground/70 text-xs">{f.key}</dt>
              <dd
                className={cn(
                  'min-w-0 truncate text-right text-xs',
                  f.ref ? 'text-primary' : 'text-foreground/90',
                  looksLikeId(f.value) && 'font-mono',
                )}
                title={f.value}
              >
                {f.value}
              </dd>
            </div>
          ))}
        </dl>
      )}

      {entity.affordances && entity.affordances.length > 0 && (
        <div className="mt-3 flex flex-wrap gap-2">
          {entity.affordances.map((a) => (
            <AffordanceButton key={a.id} affordance={a} identity={entity.identity} onAsk={onAsk} />
          ))}
        </div>
      )}
    </div>
  )
}
