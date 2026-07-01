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
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from '@/components/ui/sidebar'
import { MatrixLogo } from '@/components/matrix/matrix-logo'
import { LocaleSwitcher } from '@/components/matrix/locale-switcher'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
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
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex h-12 items-center px-2">
          <MatrixLogo />
        </div>
      </SidebarHeader>

      <SidebarContent>
        {navGroups.map((group) => (
          <SidebarGroup key={group.label}>
            <SidebarGroupLabel>{group.label}</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {group.items.map((item) => (
                  <SidebarMenuItem key={item.id}>
                    <SidebarMenuButton
                      isActive={active === item.id}
                      tooltip={item.label}
                      onClick={() => onNavigate(item.id)}
                    >
                      <item.icon />
                      <span>{item.label}</span>
                    </SidebarMenuButton>
                    {item.id === 'runs' && liveCount > 0 ? (
                      <SidebarMenuBadge>{liveCount}</SidebarMenuBadge>
                    ) : null}
                  </SidebarMenuItem>
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ))}

        <SidebarGroup>
          <SidebarGroupLabel>{t('support')}</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton
                  isActive={active === 'settings'}
                  tooltip={t('settings')}
                  onClick={() => onNavigate('settings')}
                >
                  <Settings />
                  <span>{t('settings')}</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
              <SidebarMenuItem>
                <SidebarMenuButton
                  isActive={active === 'help'}
                  tooltip={t('helpSpec')}
                  onClick={() => onNavigate('help')}
                >
                  <LifeBuoy />
                  <span>{t('helpSpec')}</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" tooltip={t('account')}>
              <Avatar className="size-7 rounded-md">
                <AvatarFallback className="bg-primary/15 text-primary rounded-md text-xs font-medium">
                  {initials}
                </AvatarFallback>
              </Avatar>
              <div className="flex flex-col text-left leading-tight">
                <span className="truncate text-sm font-medium">{displayName}</span>
                <span className="text-muted-foreground font-mono text-xs">{t('plan')}</span>
              </div>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <LocaleSwitcher />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
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
