'use client'

import { useState } from 'react'
import {
  LayoutDashboard,
  Activity,
  ReceiptText,
  Wallet,
  Bot,
  Wrench,
  Cpu,
  SlidersHorizontal,
  Settings,
  LifeBuoy,
  MoreHorizontal,
  Sparkles,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'
import { Sheet, SheetContent, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import type { DashboardView } from '@/components/matrix/dashboard-sidebar'

type Item = { id: DashboardView; icon: typeof LayoutDashboard }

const PRIMARY_IDS: Item[] = [
  { id: 'chat', icon: Sparkles },
  { id: 'runs', icon: Activity },
  { id: 'wallet', icon: Wallet },
  { id: 'receipts', icon: ReceiptText },
]

const MORE_IDS: Item[] = [
  { id: 'overview', icon: LayoutDashboard },
  { id: 'agents', icon: Bot },
  { id: 'tools', icon: Wrench },
  { id: 'models', icon: Cpu },
  { id: 'policies', icon: SlidersHorizontal },
  { id: 'settings', icon: Settings },
  { id: 'help', icon: LifeBuoy },
]

export function MobileTabBar({
  active,
  onNavigate,
  liveCount = 0,
}: {
  active: DashboardView
  onNavigate: (view: DashboardView) => void
  liveCount?: number
}) {
  const t = useTranslations('mobileTabBar')
  const [moreOpen, setMoreOpen] = useState(false)
  const moreActive = MORE_IDS.some((m) => m.id === active)

  const labelMap: Record<DashboardView, string> = {
    chat: 'Assistant',
    overview: t('home'),
    runs: t('runs'),
    wallet: t('wallet'),
    receipts: t('receipts'),
    agents: t('agents'),
    tools: t('tools'),
    models: t('models'),
    policies: t('policies'),
    settings: t('settings'),
    help: t('help'),
  }

  return (
    <>
      <nav
        aria-label={t('ariaLabel')}
        className="border-border bg-background/95 fixed inset-x-0 bottom-0 z-30 border-t pb-[env(safe-area-inset-bottom)] backdrop-blur md:hidden"
      >
        <ul className="grid grid-cols-5">
          {PRIMARY_IDS.map((tab) => (
            <li key={tab.id}>
              <button
                type="button"
                onClick={() => onNavigate(tab.id)}
                aria-current={active === tab.id ? 'page' : undefined}
                className={cn(
                  'relative flex w-full flex-col items-center gap-1 py-2 text-[13px] font-medium transition-colors',
                  active === tab.id ? 'text-primary' : 'text-muted-foreground',
                )}
              >
                <span className="relative">
                  <tab.icon className="size-6" />
                  {tab.id === 'runs' && liveCount > 0 && (
                    <span className="bg-primary text-primary-foreground absolute -top-1.5 -right-2 flex size-5 items-center justify-center rounded-full text-[11px] font-semibold">
                      {liveCount}
                    </span>
                  )}
                </span>
                {labelMap[tab.id]}
                {active === tab.id && (
                  <span
                    className="bg-primary absolute inset-x-4 top-0 h-0.5 rounded-full"
                    aria-hidden="true"
                  />
                )}
              </button>
            </li>
          ))}
          <li>
            <button
              type="button"
              onClick={() => setMoreOpen(true)}
              aria-current={moreActive ? 'page' : undefined}
              className={cn(
                'relative flex w-full flex-col items-center gap-1 py-2 text-[13px] font-medium transition-colors',
                moreActive ? 'text-primary' : 'text-muted-foreground',
              )}
            >
              <MoreHorizontal className="size-6" />
              {t('more')}
              {moreActive && (
                <span
                  className="bg-primary absolute inset-x-4 top-0 h-0.5 rounded-full"
                  aria-hidden="true"
                />
              )}
            </button>
          </li>
        </ul>
      </nav>

      <Sheet open={moreOpen} onOpenChange={setMoreOpen}>
        <SheetContent
          side="bottom"
          className="rounded-t-2xl pb-[calc(env(safe-area-inset-bottom)+1rem)]"
        >
          <SheetHeader>
            <SheetTitle>{t('more')}</SheetTitle>
          </SheetHeader>
          <div className="grid grid-cols-2 gap-3 p-4 pt-2">
            {MORE_IDS.map((item) => {
              const isActive = active === item.id
              return (
                <button
                  key={item.id}
                  type="button"
                  onClick={() => {
                    onNavigate(item.id)
                    setMoreOpen(false)
                  }}
                  className={cn(
                    'flex items-center gap-3 rounded-xl border p-4 text-left text-sm font-medium transition-colors',
                    isActive
                      ? 'border-primary/50 bg-primary/5 text-primary'
                      : 'border-border text-foreground hover:bg-accent',
                  )}
                >
                  <item.icon className="size-6" />
                  {labelMap[item.id]}
                </button>
              )
            })}
          </div>
        </SheetContent>
      </Sheet>
    </>
  )
}
