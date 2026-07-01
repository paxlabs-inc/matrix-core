'use client'

/**
 * NeoSearchResults — web-search answer + source cards.
 *
 * Surfaced from `tool.search` SSE events (the web-search MCP). Cards separate
 * from the card body by TONE (popover on card), never by stroke — and lift to
 * `muted` on hover. The "tools no other agent shows" angle, made visible.
 */
import { ExternalLink } from '@/lib/matrix-icons'
import type { NeoSearch } from '@/hooks/api/useChat'

function asHttpUrl(url: string): string {
  if (!url) return '#'
  return /^https?:\/\//i.test(url) ? url : `https://${url}`
}

function favLabel(url: string): string {
  try {
    const host = new URL(asHttpUrl(url)).hostname.replace(/^www\./, '')
    return host.slice(0, 2).toUpperCase()
  } catch {
    return (url || '··').slice(0, 2).toUpperCase()
  }
}

function prettyHost(url: string): string {
  try {
    const u = new URL(asHttpUrl(url))
    return `${u.hostname.replace(/^www\./, '')}${u.pathname}`.replace(/\/$/, '')
  } catch {
    return url
  }
}

export function NeoSearchResults({ searches }: { searches: NeoSearch[] }) {
  if (searches.length === 0) return null
  return (
    <div className="flex flex-col gap-5">
      {searches.map((search, si) => (
        <div key={`${search.query}-${si}`} className="flex flex-col gap-3">
          {search.answer ? (
            <p className="text-foreground text-[0.95rem] leading-relaxed">{search.answer}</p>
          ) : null}
          {search.results.length > 0 && (
            <>
              <p className="text-muted-foreground font-mono text-[0.65rem] tracking-[0.14em] uppercase">
                {search.results.length} {search.results.length === 1 ? 'source' : 'sources'}
              </p>
              <div className="flex flex-col gap-2">
                {search.results.map((r, ri) => (
                  <a
                    key={`${r.url}-${ri}`}
                    href={asHttpUrl(r.url)}
                    target="_blank"
                    rel="noreferrer noopener"
                    className="bg-popover hover:bg-muted group flex gap-3 rounded-2xl p-3 transition-colors"
                  >
                    <span className="bg-muted group-hover:bg-accent text-muted-foreground flex size-9 shrink-0 items-center justify-center rounded-xl font-mono text-xs font-semibold transition-colors">
                      {favLabel(r.url)}
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="text-foreground line-clamp-1 block text-sm font-semibold">
                        {r.title || prettyHost(r.url)}
                      </span>
                      {r.snippet ? (
                        <span className="text-muted-foreground mt-0.5 line-clamp-2 block text-xs leading-snug">
                          {r.snippet}
                        </span>
                      ) : null}
                      <span className="text-primary mt-1 flex items-center gap-1 font-mono text-[0.65rem]">
                        <ExternalLink className="size-3 shrink-0" />
                        <span className="truncate">{prettyHost(r.url)}</span>
                      </span>
                    </span>
                  </a>
                ))}
              </div>
            </>
          )}
        </div>
      ))}
    </div>
  )
}
