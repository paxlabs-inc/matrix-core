'use client'

import { useState } from 'react'
import { useTranslations } from 'next-intl'
import { ExternalLink } from '@/lib/matrix-icons'
import type { FinanceResearchKind, ResearchCitation, ResearchGrounding } from '@/lib/api/finance'
import {
  useCancelFinanceResearch,
  useExtractFinanceNews,
  useFinanceResearch,
  useStartFinanceResearch,
  useVerifyFinanceFacts,
} from '@/hooks/api/useFinance'

type ResearchMode =
  | { kind: FinanceResearchKind; fields?: never; dimensions?: string[]; urls?: never }
  | { kind?: never; fields: string[]; dimensions?: never; urls?: never }
  | { kind?: never; fields?: never; dimensions?: never; urls: string[] }

interface GroundedResearchProps {
  symbol: string
  title: string
  description: string
  actionLabel: string
  mode: ResearchMode
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback
}

function decodeGenerated(value: unknown): unknown {
  if (typeof value !== 'string') return value
  try {
    return JSON.parse(value) as unknown
  } catch {
    return value
  }
}

function labelFor(key: string): string {
  return key.replaceAll('_', ' ').replace(/\b\w/g, (letter) => letter.toUpperCase())
}

function ResearchValue({ value, depth = 0 }: { value: unknown; depth?: number }) {
  const decoded = decodeGenerated(value)
  if (decoded === null || decoded === undefined || decoded === '') return <span>—</span>
  if (typeof decoded === 'string' || typeof decoded === 'number' || typeof decoded === 'boolean') {
    return <span className="text-foreground/90 whitespace-pre-wrap">{String(decoded)}</span>
  }
  if (Array.isArray(decoded)) {
    return (
      <div className="flex flex-col gap-1.5">
        {decoded.map((item, index) => (
          <div key={index} className="bg-background/50 rounded-lg px-2.5 py-2">
            <ResearchValue value={item} depth={depth + 1} />
          </div>
        ))}
      </div>
    )
  }
  if (typeof decoded === 'object') {
    const entries = Object.entries(decoded as Record<string, unknown>)
    return (
      <dl className="flex flex-col gap-2">
        {entries.map(([key, item]) => (
          <div key={key} className={depth === 0 ? 'bg-background/50 rounded-lg px-2.5 py-2' : ''}>
            <dt className="text-muted-foreground mb-0.5 text-[0.65rem] font-medium tracking-wide uppercase">
              {labelFor(key)}
            </dt>
            <dd className="text-xs leading-relaxed">
              <ResearchValue value={item} depth={depth + 1} />
            </dd>
          </div>
        ))}
      </dl>
    )
  }
  return <span>{String(decoded)}</span>
}

function citationsFrom(grounding: ResearchGrounding[] = [], fallbacks: ResearchCitation[] = []) {
  const byURL = new Map<string, ResearchCitation>()
  for (const group of grounding) {
    for (const citation of group.citations ?? []) {
      if (citation.url) byURL.set(citation.url, citation)
    }
  }
  for (const citation of fallbacks) {
    if (citation.url && !byURL.has(citation.url)) byURL.set(citation.url, citation)
  }
  return [...byURL.values()]
}

export function GroundedResearch({
  symbol,
  title,
  description,
  actionLabel,
  mode,
}: GroundedResearchProps) {
  const t = useTranslations('finance')
  const [runId, setRunId] = useState<string | null>(null)
  const start = useStartFinanceResearch()
  const run = useFinanceResearch(runId)
  const cancel = useCancelFinanceResearch()
  const verify = useVerifyFinanceFacts()
  const extract = useExtractFinanceNews()

  const envelope = run.data ?? start.data
  const status = envelope?.run.status
  const output = envelope?.run.output
  const verification = verify.data
  const newsEvidence = extract.data
  const generated =
    output?.structured ?? output?.content ?? output?.text ?? verification?.data.output?.content
  const extractive = newsEvidence?.data.results.map((result) => ({
    source: result.title || result.url,
    evidence: result.highlights ?? [],
  }))
  const displayed = generated ?? extractive
  const grounding = output?.grounding ?? verification?.data.output?.grounding ?? []
  const fallbackCitations =
    verification?.data.results.map((result) => ({ url: result.url, title: result.title })) ??
    newsEvidence?.data.results.map((result) => ({ url: result.url, title: result.title })) ??
    []
  const citations = citationsFrom(grounding, fallbackCitations)
  const pending = start.isPending || verify.isPending || extract.isPending
  const active = status === 'queued' || status === 'running'
  const error = start.error ?? run.error ?? verify.error ?? extract.error ?? cancel.error
  const partialStatuses =
    newsEvidence?.data.statuses.filter((item) => item.status !== 'success') ?? []
  const retrievedAt =
    envelope?.meta.retrieved_at ??
    verification?.meta.retrieved_at ??
    newsEvidence?.meta.retrieved_at
  const cost =
    envelope?.run.costDollars?.total ??
    verification?.data.costDollars?.total ??
    newsEvidence?.data.costDollars?.total

  const launch = () => {
    if (mode.kind) {
      start.mutate(
        { kind: mode.kind, symbol, dimensions: mode.dimensions },
        { onSuccess: (result) => setRunId(result.run.id) },
      )
      return
    }
    if (mode.fields) {
      verify.mutate({ symbol, fields: mode.fields })
      return
    }
    extract.mutate({ symbol, urls: mode.urls })
  }

  return (
    <section
      className="bg-muted/30 flex flex-col gap-3 rounded-xl p-3"
      data-testid="grounded-research"
    >
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <h3 className="text-foreground text-sm font-medium">{title}</h3>
          <p className="text-muted-foreground mt-0.5 text-xs leading-relaxed">{description}</p>
        </div>
        <button
          type="button"
          onClick={launch}
          disabled={pending || active || (Array.isArray(mode.urls) && mode.urls.length === 0)}
          className="bg-foreground text-background hover:bg-foreground/85 disabled:bg-muted-foreground/30 shrink-0 rounded-lg px-3 py-1.5 text-xs font-medium transition-colors disabled:cursor-not-allowed"
        >
          {pending ? t('researchStarting') : actionLabel}
        </button>
      </div>

      {status === 'queued' ? (
        <p className="text-muted-foreground text-xs">{t('researchQueued')}</p>
      ) : null}
      {status === 'running' ? (
        <div className="flex items-center justify-between gap-3">
          <p className="text-muted-foreground text-xs">{t('researchRunning')}</p>
          <button
            type="button"
            onClick={() => runId && cancel.mutate(runId)}
            disabled={cancel.isPending}
            className="bg-background text-muted-foreground hover:text-foreground rounded-md px-2 py-1 text-[0.68rem] transition-colors"
          >
            {t('cancelResearch')}
          </button>
        </div>
      ) : null}
      {status === 'cancelled' ? (
        <p className="text-muted-foreground text-xs">{t('researchCancelled')}</p>
      ) : null}
      {status === 'failed' ? (
        <p className="text-destructive text-xs">
          {envelope?.run.error?.message || t('researchFailed')}
        </p>
      ) : null}
      {error ? (
        <p className="text-destructive text-xs">{errorMessage(error, t('researchFailed'))}</p>
      ) : null}

      {displayed !== undefined ? (
        <div className="flex flex-col gap-2">
          <p className="text-muted-foreground text-[0.68rem]">
            {generated !== undefined ? t('generatedSynthesis') : t('extractiveEvidence')}
          </p>
          <ResearchValue value={displayed} />
        </div>
      ) : null}

      {partialStatuses.length > 0 ? (
        <div className="bg-background/50 rounded-lg px-2.5 py-2">
          <p className="text-muted-foreground text-[0.68rem] font-medium">{t('partialEvidence')}</p>
          {partialStatuses.map((item) => (
            <p key={item.id} className="text-destructive mt-1 text-xs">
              {item.id} — {item.error?.tag || item.status}
            </p>
          ))}
        </div>
      ) : null}

      {citations.length > 0 ? (
        <div className="flex flex-col gap-1.5">
          <p className="text-muted-foreground text-[0.68rem] font-medium uppercase">
            {t('evidenceSources')}
          </p>
          <div className="flex flex-col gap-1">
            {citations.map((citation) => (
              <a
                key={citation.url}
                href={citation.url}
                target="_blank"
                rel="noreferrer"
                className="bg-background/50 text-foreground hover:bg-background flex items-center gap-2 rounded-lg px-2.5 py-2 text-xs transition-colors"
              >
                <span className="min-w-0 flex-1 truncate">{citation.title || citation.url}</span>
                <ExternalLink
                  className="text-muted-foreground size-3.5 shrink-0"
                  aria-hidden="true"
                />
              </a>
            ))}
          </div>
        </div>
      ) : null}
      {retrievedAt || cost !== undefined ? (
        <p className="text-muted-foreground text-[0.65rem]">
          Exa
          {retrievedAt ? ` · ${t('updated')} ${new Date(retrievedAt).toLocaleString()}` : ''}
          {cost !== undefined ? ` · $${cost.toFixed(3)}` : ''}
        </p>
      ) : null}
    </section>
  )
}
