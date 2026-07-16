'use client'

/**
 * LayerX explorer shell — header, tab navigation and the universal search
 * box (seq, DID, batch id, batch root). Wraps every /explorer page.
 */
import { useState, type FormEvent, type ReactNode } from 'react'
import { useTranslations } from 'next-intl'
import { Link, usePathname, useRouter } from '@/i18n/navigation'
import { Search } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import { getLayerXAnchor } from '@/lib/layerx/explorer'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const ROOT_RE = /^(0x)?[0-9a-f]{64}$/i

export function ExplorerShell({ children }: { children: ReactNode }) {
  const t = useTranslations('layerxExplorer')
  const pathname = usePathname()
  const router = useRouter()
  const [query, setQuery] = useState('')
  const [notFound, setNotFound] = useState(false)
  const [searching, setSearching] = useState(false)

  const tabs = [
    { href: '/explorer', label: t('tabOverview') },
    { href: '/explorer/transfers', label: t('tabTransfers') },
    { href: '/explorer/batches', label: t('tabBatches') },
  ] as const

  async function onSearch(e: FormEvent) {
    e.preventDefault()
    const q = query.trim()
    if (!q || searching) return
    setNotFound(false)
    if (/^\d+$/.test(q)) {
      router.push(`/explorer/tx/${q}`)
      return
    }
    if (q.startsWith('did:')) {
      router.push(`/explorer/account/${encodeURIComponent(q)}`)
      return
    }
    if (UUID_RE.test(q)) {
      router.push(`/explorer/batch/${q}`)
      return
    }
    if (ROOT_RE.test(q)) {
      setSearching(true)
      try {
        const anchor = await getLayerXAnchor(q)
        router.push(`/explorer/batch/${anchor.batch_id}`)
      } catch {
        setNotFound(true)
      } finally {
        setSearching(false)
      }
      return
    }
    setNotFound(true)
  }

  return (
    <div className="bg-background min-h-dvh">
      <div className="mx-auto flex w-full max-w-5xl flex-col gap-4 px-4 py-6 sm:px-6">
        <header className="flex flex-col gap-4">
          <div className="flex flex-wrap items-end justify-between gap-3">
            <div className="flex flex-col gap-0.5">
              <h1 className="text-foreground text-lg font-semibold">{t('title')}</h1>
              <p className="text-muted-foreground text-xs">{t('subtitle')}</p>
            </div>
            <Link
              href="/"
              className="bg-card text-muted-foreground hover:bg-surface-hover hover:text-foreground rounded-lg px-3 py-1.5 text-xs font-medium transition-colors"
            >
              {t('backToApp')}
            </Link>
          </div>

          <form onSubmit={onSearch} className="flex flex-col gap-1.5">
            <div className="bg-card focus-within:bg-surface-hover flex items-center gap-2.5 rounded-xl px-3.5 py-2.5 transition-colors">
              <Search className="text-muted-foreground size-4 shrink-0" />
              <input
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value)
                  setNotFound(false)
                }}
                placeholder={t('searchPlaceholder')}
                spellCheck={false}
                className="text-foreground placeholder:text-muted-foreground/60 w-full bg-transparent font-mono text-sm outline-none"
              />
              {searching && (
                <span className="text-muted-foreground shrink-0 text-xs">{t('searching')}</span>
              )}
            </div>
            {notFound && <p className="text-destructive px-1 text-xs">{t('searchNotFound')}</p>}
          </form>

          <nav className="bg-card flex w-fit items-center gap-1 rounded-xl p-1">
            {tabs.map((tab) => {
              const active =
                tab.href === '/explorer' ? pathname === '/explorer' : pathname.startsWith(tab.href)
              return (
                <Link
                  key={tab.href}
                  href={tab.href}
                  className={cn(
                    'rounded-lg px-3.5 py-1.5 text-xs font-medium transition-colors',
                    active
                      ? 'bg-surface-hover text-foreground'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {tab.label}
                </Link>
              )
            })}
          </nav>
        </header>

        <main className="flex flex-col gap-4">{children}</main>
      </div>
    </div>
  )
}
