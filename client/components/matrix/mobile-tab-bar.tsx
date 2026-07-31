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
import { Button } from '@astryxdesign/core/Button'
import { Badge } from '@astryxdesign/core/Badge'
import { MobileNav } from '@astryxdesign/core/MobileNav'
import { SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav'
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
        className="bg-background/95 fixed inset-x-0 bottom-0 z-30 pb-[env(safe-area-inset-bottom)] backdrop-blur md:hidden"
      >
        <ul className="grid grid-cols-5">
          {PRIMARY_IDS.map((tab) => (
            <li key={tab.id} className="p-1">
              <Button
                label={labelMap[tab.id]}
                icon={<tab.icon className="size-5" />}
                variant={active === tab.id ? 'primary' : 'ghost'}
                size="lg"
                width="100%"
                onClick={() => onNavigate(tab.id)}
                endContent={
                  tab.id === 'runs' && liveCount > 0 ? (
                    <Badge variant="info" label={liveCount} />
                  ) : undefined
                }
              />
            </li>
          ))}
          <li className="p-1">
            <Button
              label={t('more')}
              icon={<MoreHorizontal className="size-5" />}
              variant={moreActive ? 'primary' : 'ghost'}
              size="lg"
              width="100%"
              onClick={() => setMoreOpen(true)}
            />
          </li>
        </ul>
      </nav>

      <MobileNav
        isOpen={moreOpen}
        onOpenChange={setMoreOpen}
        header={t('more')}
        side="end"
        label={t('more')}
      >
        <SideNavSection title={t('more')}>
          {MORE_IDS.map((item) => (
            <SideNavItem
              key={item.id}
              label={labelMap[item.id]}
              icon={<item.icon />}
              isSelected={active === item.id}
              onClick={() => {
                onNavigate(item.id)
                setMoreOpen(false)
              }}
            />
          ))}
        </SideNavSection>
      </MobileNav>
    </>
  )
}
