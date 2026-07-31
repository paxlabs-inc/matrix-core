'use client'

import { Plus, Wallet, SlidersHorizontal, ArrowRight, type LucideIcon } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/matrix/astryx-card'
import { RunCard } from '@/components/matrix/run-card'
import { ActivityTimeline } from '@/components/matrix/activity-timeline'
import { DispatchDialog, type DispatchPayload } from '@/components/matrix/dispatch-dialog'
import { cn } from '@/lib/utils'
import type { DashboardView } from '@/components/matrix/dashboard-sidebar'
import {
  activity as fallbackActivity,
  stats as fallbackStats,
  type Run,
  type ActivityDay,
  type Stats,
  type Agent,
  type Tool,
} from '@/lib/matrix-data'

export function OverviewPanel({
  userName,
  liveRuns,
  onDispatch,
  onPauseResume,
  onViewReceipt,
  onOpenTranscript,
  onNavigate,
  activity = fallbackActivity,
  stats = fallbackStats,
  agents,
  tools,
}: {
  userName: string
  liveRuns: Run[]
  onDispatch: (payload: DispatchPayload) => void
  onPauseResume: (run: Run) => void
  onViewReceipt: (run: Run) => void
  onOpenTranscript?: (run: Run) => void
  onNavigate: (view: DashboardView) => void
  activity?: ActivityDay[]
  stats?: Stats
  agents?: Agent[]
  tools?: Tool[]
}) {
  const t = useTranslations('overview')
  const rawFirst = userName.split(' ')[0] ?? userName
  const firstName = rawFirst ? rawFirst.charAt(0).toUpperCase() + rawFirst.slice(1) : userName

  const h = new Date().getHours()
  const greetingKey = h < 12 ? 'morning' : h < 18 ? 'afternoon' : 'evening'

  const glance = [
    {
      value: `${stats.activeNow}`,
      label: t('agentsWorking', { count: stats.activeNow }),
    },
    { value: `${stats.completionRate}%`, label: t('completionRate') },
    { value: `${stats.timeReclaimedHrs}h`, label: t('timeSaved') },
  ]

  return (
    <div className="flex flex-col gap-6">
      {/* Greeting */}
      <div>
        <h2 className="text-foreground text-2xl font-semibold tracking-tight text-balance">
          {t(greetingKey)}, {firstName}.
        </h2>
        <p className="text-muted-foreground mt-1 text-pretty">
          {liveRuns.length > 0 ? t('description', { count: liveRuns.length }) : t('noTasks')}
        </p>
      </div>

      {/* Quick actions */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <DispatchDialog
          onDispatch={onDispatch}
          agents={agents}
          tools={tools}
          trigger={
            <button type="button" className="text-left">
              <QuickAction icon={Plus} title={t('startTask')} desc={t('startTaskDesc')} accent />
            </button>
          }
        />
        <button type="button" onClick={() => onNavigate('wallet')} className="text-left">
          <QuickAction icon={Wallet} title={t('checkWallet')} desc={t('checkWalletDesc')} />
        </button>
        <button type="button" onClick={() => onNavigate('policies')} className="text-left">
          <QuickAction
            icon={SlidersHorizontal}
            title={t('tuneGuardrails')}
            desc={t('tuneGuardrailsDesc')}
          />
        </button>
      </div>

      {/* At a glance */}
      <Card>
        <CardContent className="divide-border grid grid-cols-1 divide-y p-0 sm:grid-cols-3 sm:divide-x sm:divide-y-0">
          {glance.map((g) => (
            <div key={g.label} className="flex items-baseline gap-2 p-5">
              <span className="text-foreground text-2xl font-semibold tracking-tight">
                {g.value}
              </span>
              <span className="text-muted-foreground text-sm text-pretty">{g.label}</span>
            </div>
          ))}
        </CardContent>
      </Card>

      {/* Activity + in flight */}
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
        <div className="lg:col-span-2">
          <ActivityTimeline days={activity} />
        </div>
        <div className="flex flex-col gap-4">
          <div className="flex items-center justify-between">
            <h2 className="text-foreground text-sm font-medium">{t('inFlight')}</h2>
            <button
              type="button"
              onClick={() => onNavigate('runs')}
              className="text-muted-foreground hover:text-foreground flex items-center gap-1 text-xs font-medium transition-colors"
            >
              {t('seeAll')}
              <ArrowRight className="size-3.5" />
            </button>
          </div>
          {liveRuns.length === 0 ? (
            <Card>
              <CardContent className="text-muted-foreground p-6 text-center text-sm">
                {t('noInFlight')}
              </CardContent>
            </Card>
          ) : (
            liveRuns
              .slice(0, 3)
              .map((run) => (
                <RunCard
                  key={run.id}
                  run={run}
                  onPauseResume={onPauseResume}
                  onViewReceipt={onViewReceipt}
                  onOpenTranscript={onOpenTranscript}
                />
              ))
          )}
        </div>
      </div>
    </div>
  )
}

function QuickAction({
  icon: Icon,
  title,
  desc,
  accent,
}: {
  icon: LucideIcon
  title: string
  desc: string
  accent?: boolean
}) {
  return (
    <div
      className={cn(
        'flex h-full items-start gap-3 rounded-2xl border p-4 transition-colors',
        accent
          ? 'border-primary/40 bg-primary/5 hover:bg-primary/10'
          : 'border-border bg-card hover:bg-accent',
      )}
    >
      <span
        className={cn(
          'flex size-9 shrink-0 items-center justify-center rounded-xl',
          accent ? 'bg-primary text-primary-foreground' : 'bg-muted text-foreground',
        )}
      >
        <Icon className="size-4" />
      </span>
      <div className="min-w-0">
        <p className="text-foreground text-sm font-medium">{title}</p>
        <p className="text-muted-foreground text-xs text-pretty">{desc}</p>
      </div>
    </div>
  )
}
