'use client'

import { CheckCircle2, Gauge, Loader2, Clock } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/matrix/astryx-card'
import { NumberFlow } from '@/components/matrix/primitives'
import type { Stats } from '@/lib/matrix-data'

export function StatCards({ stats }: { stats: Stats }) {
  const t = useTranslations('statCards')

  const items = [
    {
      label: t('tasksCompleted'),
      icon: CheckCircle2,
      node: <NumberFlow value={stats.tasksCompleted} className="tabular-nums" />,
      hint: t('allTime'),
    },
    {
      label: t('completionRate'),
      icon: Gauge,
      node: <NumberFlow value={stats.completionRate} suffix="%" className="tabular-nums" />,
      hint: t('last30Days'),
    },
    {
      label: t('activeNow'),
      icon: Loader2,
      node: <NumberFlow value={stats.activeNow} className="tabular-nums" />,
      hint: t('running'),
    },
    {
      label: t('timeReclaimed'),
      icon: Clock,
      node: <NumberFlow value={stats.timeReclaimedHrs} suffix="h" className="tabular-nums" />,
      hint: t('estThisQuarter'),
    },
  ]

  return (
    <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
      {items.map((item) => (
        <Card key={item.label}>
          <CardContent className="flex flex-col gap-3 p-5">
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground text-sm">{item.label}</span>
              <item.icon className="text-muted-foreground size-4" />
            </div>
            <div className="text-foreground text-3xl font-semibold tracking-tight">{item.node}</div>
            <span className="text-muted-foreground font-mono text-xs">{item.hint}</span>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}
