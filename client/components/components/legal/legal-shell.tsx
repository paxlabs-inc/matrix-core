'use client'

import { Link, usePathname } from '@/i18n/navigation'
import { cn } from '@/lib/utils'
import type { ReactNode } from 'react'

const NAV = [
  { href: '/legal', label: 'Legal Home' },
  { href: '/terms', label: 'Terms' },
  { href: '/privacy', label: 'Privacy' },
  { href: '/acceptable-use', label: 'Acceptable Use' },
  { href: '/cookies', label: 'Cookies' },
  { href: '/risk-disclosure', label: 'Risk' },
] as const

export function LegalShell({ children }: { children: ReactNode }) {
  const pathname = usePathname()

  return (
    <div className="bg-background text-foreground min-h-screen">
      <header className="border-border/60 bg-background/95 sticky top-0 z-40 border-b backdrop-blur">
        <div className="mx-auto flex max-w-6xl items-center justify-between gap-4 px-6 py-4">
          <Link href="/legal" className="flex items-center gap-3 no-underline">
            <span className="bg-primary text-primary-foreground flex size-9 items-center justify-center rounded-lg text-lg font-bold">
              M
            </span>
            <span className="leading-tight">
              <span className="block font-semibold">Matrix</span>
              <span className="text-muted-foreground block text-xs tracking-wide uppercase">
                Paxeer Network
              </span>
            </span>
          </Link>
          <nav className="hidden flex-wrap justify-end gap-1 sm:flex" aria-label="Legal documents">
            {NAV.map((item) => {
              const active = pathname === item.href
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  className={cn(
                    'rounded-md px-3 py-1.5 text-sm transition-colors',
                    active
                      ? 'bg-accent text-foreground font-medium'
                      : 'text-muted-foreground hover:bg-accent hover:text-foreground',
                  )}
                >
                  {item.label}
                </Link>
              )
            })}
          </nav>
        </div>
      </header>
      {children}
    </div>
  )
}
