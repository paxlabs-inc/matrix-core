'use client'

import { Link, usePathname } from '@/i18n/navigation'
import type { ReactNode } from 'react'
import { Layout } from '@astryxdesign/core/Layout'
import { TopNav, TopNavHeading, TopNavItem } from '@astryxdesign/core/TopNav'

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
    <Layout
      height="auto"
      padding={0}
      className="bg-background text-foreground min-h-screen"
      header={
        <TopNav
          label="Legal documents"
          heading={
            <TopNavHeading
              as={Link}
              logo={
                <span className="bg-primary text-primary-foreground flex size-9 items-center justify-center rounded-lg text-lg font-bold">
                  M
                </span>
              }
              heading="Centra AI"
              headingHref="/legal"
              subheading="Paxeer Network"
            />
          }
          endContent={
            <>
              {NAV.map((item) => {
                const active = pathname === item.href
                return (
                  <TopNavItem
                    key={item.href}
                    as={Link}
                    href={item.href}
                    label={item.label}
                    isSelected={active}
                  />
                )
              })}
            </>
          }
        />
      }
      content={children}
    />
  )
}
