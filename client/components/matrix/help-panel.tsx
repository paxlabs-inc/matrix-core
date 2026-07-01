'use client'

import {
  BookIcon,
  ExternalLink,
  FileText,
  LifeBuoy,
  ReceiptText,
  ShieldCheck,
  Sparkles,
  type LucideIcon,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/ui/card'
import { Link } from '@/i18n/navigation'
import { routerOrigin } from '@/lib/env'
import type { DashboardView } from '@/components/matrix/dashboard-sidebar'

type HelpLink = {
  label: string
  description: string
  icon: LucideIcon
  href?: string
  external?: boolean
  view?: DashboardView
}

export function HelpPanel({ onNavigate }: { onNavigate: (view: DashboardView) => void }) {
  const t = useTranslations('helpPanel')
  const router = routerOrigin()

  const topics: HelpLink[] = [
    {
      label: t('topics.assistant.label'),
      description: t('topics.assistant.description'),
      icon: Sparkles,
      view: 'chat',
    },
    {
      label: t('topics.receipts.label'),
      description: t('topics.receipts.description'),
      icon: ReceiptText,
      view: 'receipts',
    },
    {
      label: t('topics.policies.label'),
      description: t('topics.policies.description'),
      icon: ShieldCheck,
      view: 'policies',
    },
  ]

  const resources: HelpLink[] = [
    {
      label: t('resources.api.label'),
      description: t('resources.api.description'),
      icon: BookIcon,
      href: `${router}/openapi.json`,
      external: true,
    },
    {
      label: t('resources.legal.label'),
      description: t('resources.legal.description'),
      icon: FileText,
      href: '/legal',
    },
    {
      label: t('resources.terms.label'),
      description: t('resources.terms.description'),
      icon: FileText,
      href: '/terms',
    },
    {
      label: t('resources.privacy.label'),
      description: t('resources.privacy.description'),
      icon: LifeBuoy,
      href: '/privacy',
    },
  ]

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <Card>
        <CardContent className="flex flex-col gap-3 p-5">
          <h2 className="text-foreground text-sm font-medium">{t('introTitle')}</h2>
          <p className="text-muted-foreground text-sm leading-relaxed">{t('introBody')}</p>
        </CardContent>
      </Card>

      <Section title={t('topicsTitle')}>
        {topics.map((item) => (
          <HelpRow key={item.label} item={item} onNavigate={onNavigate} />
        ))}
      </Section>

      <Section title={t('resourcesTitle')}>
        {resources.map((item) => (
          <HelpRow key={item.label} item={item} onNavigate={onNavigate} />
        ))}
      </Section>
    </div>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardContent className="flex flex-col gap-4 p-5">
        <h2 className="text-foreground text-sm font-medium">{title}</h2>
        {children}
      </CardContent>
    </Card>
  )
}

function HelpRow({
  item,
  onNavigate,
}: {
  item: HelpLink
  onNavigate: (view: DashboardView) => void
}) {
  const Icon = item.icon

  const content = (
    <>
      <span className="bg-muted text-muted-foreground flex size-10 shrink-0 items-center justify-center rounded-xl">
        <Icon className="size-5" />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm font-medium">{item.label}</p>
        <p className="text-muted-foreground text-sm">{item.description}</p>
      </div>
      {item.external ? <ExternalLink className="text-muted-foreground size-4 shrink-0" /> : null}
    </>
  )

  if (item.view) {
    return (
      <button
        type="button"
        onClick={() => onNavigate(item.view!)}
        className="hover:bg-accent flex w-full items-center gap-3 rounded-lg p-2 text-left transition-colors"
      >
        {content}
      </button>
    )
  }

  if (item.href && item.external) {
    return (
      <a
        href={item.href}
        target="_blank"
        rel="noopener noreferrer"
        className="hover:bg-accent flex items-center gap-3 rounded-lg p-2 transition-colors"
      >
        {content}
      </a>
    )
  }

  if (item.href) {
    return (
      <Link
        href={item.href}
        className="hover:bg-accent flex items-center gap-3 rounded-lg p-2 transition-colors"
      >
        {content}
      </Link>
    )
  }

  return <div className="flex items-center gap-3 p-2">{content}</div>
}
