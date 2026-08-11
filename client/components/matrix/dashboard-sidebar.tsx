'use client'

import {
  LayoutDashboard,
  Activity,
  BadgeCheck,
  Bot,
  Wrench,
  Cpu,
  Wallet,
  SlidersHorizontal,
  Settings,
  LifeBuoy,
  Sparkles,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { SideNav, SideNavHeading, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav'
import { Badge } from '@astryxdesign/core/Badge'
import { CentraLogo } from '@/components/brand/centra-logo'
import { BRAND_NAME } from '@/lib/brand'
import { LocaleSwitcher } from '@/components/matrix/locale-switcher'
export type DashboardView =
  | 'chat'
  | 'overview'
  | 'runs'
  | 'receipts'
  | 'agents'
  | 'tools'
  | 'models'
  | 'wallet'
  | 'policies'
  | 'settings'
  | 'help'

export function DashboardSidebar({
  active,
  onNavigate,
  liveCount = 0,
  userLabel = '',
}: {
  active: DashboardView
  onNavigate: (view: DashboardView) => void
  liveCount?: number
  /** Current signed-in user label, resolved upstream from the daemon. */
  userLabel?: string
}) {
  const t = useTranslations('dashboardSidebar')
  const displayName = userLabel.trim() || t('account')
  const initials = initialsFrom(displayName)

  type NavItem = {
    id: DashboardView
    label: string
    icon: React.ComponentType<{ className?: string }>
  }

  const navGroups: { label: string; items: NavItem[] }[] = [
    {
      label: t('workspace'),
      items: [
        { id: 'chat', label: t('assistant'), icon: Sparkles },
        { id: 'overview', label: t('overview'), icon: LayoutDashboard },
        { id: 'runs', label: t('activeRuns'), icon: Activity },
        { id: 'receipts', label: t('receipts'), icon: BadgeCheck },
      ],
    },
    {
      label: t('build'),
      items: [
        { id: 'agents', label: t('agents'), icon: Bot },
        { id: 'tools', label: t('tools'), icon: Wrench },
        { id: 'models', label: t('models'), icon: Cpu },
      ],
    },
    {
      label: t('onchain'),
      items: [
        { id: 'wallet', label: t('wallet'), icon: Wallet },
        { id: 'policies', label: t('policies'), icon: SlidersHorizontal },
      ],
    },
  ]

  return (
    <SideNav
      header={<SideNavHeading icon={<CentraLogo iconOnly />} heading={BRAND_NAME} />}
      collapsible={{ buttonLabel: t('collapseSidebar') }}
      footer={
        <>
          <SideNavItem
            label={displayName}
            icon={
              <span className="bg-primary/15 text-primary grid size-7 place-items-center rounded-md text-xs font-medium">
                {initials}
              </span>
            }
            endContent={<Badge variant="neutral" label={t('plan')} />}
          />
          <LocaleSwitcher />
        </>
      }
    >
      {navGroups.map((group) => (
        <SideNavSection key={group.label} title={group.label}>
          {group.items.map((item) => (
            <SideNavItem
              key={item.id}
              label={item.label}
              icon={<item.icon />}
              isSelected={active === item.id}
              onClick={() => onNavigate(item.id)}
              endContent={
                item.id === 'runs' && liveCount > 0 ? (
                  <Badge variant="info" label={liveCount} />
                ) : undefined
              }
            />
          ))}
        </SideNavSection>
      ))}

      <SideNavSection title={t('support')}>
        <SideNavItem
          label={t('settings')}
          icon={<Settings />}
          isSelected={active === 'settings'}
          onClick={() => onNavigate('settings')}
        />
        <SideNavItem
          label={t('helpSpec')}
          icon={<LifeBuoy />}
          isSelected={active === 'help'}
          onClick={() => onNavigate('help')}
        />
      </SideNavSection>
    </SideNav>
  )
}

/** Up-to-two-letter avatar initials from a display label. Handles
 *  "Firstname Lastname", single words, emails, and "User a1b2c3d4". */
function initialsFrom(label: string): string {
  const cleaned = label.trim()
  if (!cleaned) return 'MX'
  const atIndex = cleaned.indexOf('@')
  const base = atIndex > 0 ? cleaned.slice(0, atIndex) : cleaned
  const words = base.split(/[\s._-]+/).filter(Boolean)
  if (words.length >= 2) return (words[0][0] + words[1][0]).toUpperCase()
  return base.slice(0, 2).toUpperCase()
}
