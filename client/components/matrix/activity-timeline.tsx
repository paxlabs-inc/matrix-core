'use client'

/**
 * ActivityTimeline — a friendly, narrative feed of what the agents have been
 * doing, grouped by day with a sticky date header that tracks the scroll.
 *
 * Adapted from Skiper UI "Skiper74" (sticky-scroll timeline). Re-themed to the
 * Centra AI design tokens and driven entirely by the `days` prop, so a backend
 * can supply real activity without touching this component.
 */

import { motion, useInView } from 'motion/react'
import { useEffect, useRef, useState } from 'react'
import {
  CheckCircle2,
  Rocket,
  PauseCircle,
  Coins,
  AlertTriangle,
  type LucideIcon,
} from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'
import type { ActivityDay, ActivityItem, ActivityKind } from '@/lib/matrix-data'

const KIND_META: Record<ActivityKind, { icon: LucideIcon; className: string }> = {
  completed: { icon: CheckCircle2, className: 'bg-primary/12 text-primary' },
  dispatched: { icon: Rocket, className: 'bg-sky-500/12 text-sky-500' },
  needs_input: { icon: PauseCircle, className: 'bg-amber-500/12 text-amber-500' },
  onchain: { icon: Coins, className: 'bg-[#004ced]/12 text-[#004ced]' },
  failed: { icon: AlertTriangle, className: 'bg-destructive/12 text-destructive' },
}

export function ActivityTimeline({ days }: { days: ActivityDay[] }) {
  const t = useTranslations('activityTimeline')
  const [currentDate, setCurrentDate] = useState(days[0]?.date ?? '')

  const [lead, ...rest] = currentDate.split(' / ')

  return (
    <section className="border-border bg-card rounded-2xl border">
      {/* Sticky day header */}
      <header className="border-border bg-card/95 sticky top-14 z-10 flex items-baseline gap-2 rounded-t-2xl border-b px-5 py-3.5 text-sm backdrop-blur">
        <span className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
          {t('recentActivity')}
        </span>
        <span className="ml-auto flex items-baseline gap-1.5">
          <motion.span
            key={currentDate}
            initial={{ x: -4, opacity: 0 }}
            animate={{ x: 0, opacity: 1 }}
            className="text-foreground font-medium"
          >
            {lead}
          </motion.span>
          {rest.length > 0 && <span className="text-muted-foreground">/ {rest.join(' / ')}</span>}
        </span>
      </header>

      <div className="px-5 pb-2">
        {days.map((day) => (
          <DayBlock key={day.date} day={day} onActive={setCurrentDate} />
        ))}
      </div>
    </section>
  )
}

function DayBlock({ day, onActive }: { day: ActivityDay; onActive: (d: string) => void }) {
  const ref = useRef<HTMLDivElement>(null)
  const inView = useInView(ref, { amount: 0, margin: '-80px 0px -70% 0px' })

  useEffect(() => {
    if (inView) onActive(day.date)
  }, [inView, day.date, onActive])

  return (
    <div ref={ref} className="py-5">
      <h3 className="border-border/60 text-muted-foreground mb-3 border-b pb-2 text-sm font-medium tracking-tight">
        <span className="text-foreground mr-1">{day.date.split(' / ')[0]}</span>
        {day.date.includes(' / ') && <>/ {day.date.split(' / ').slice(1).join(' / ')}</>}
      </h3>

      <ul className="flex flex-col">
        {day.items.map((item) => (
          <Row key={item.id} item={item} />
        ))}
      </ul>
    </div>
  )
}

function Row({ item }: { item: ActivityItem }) {
  const t = useTranslations('activityTimeline')
  const meta = KIND_META[item.kind]
  const Icon = meta.icon

  const kindLabel: Record<ActivityKind, string> = {
    completed: t('completed'),
    dispatched: t('dispatched'),
    needs_input: t('needsInput'),
    onchain: t('onchain'),
    failed: t('failed'),
  }

  return (
    <li className="group hover:bg-accent/60 flex items-start gap-3 rounded-xl px-2 py-2.5 transition-colors">
      <span
        className={cn(
          'mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full',
          meta.className,
        )}
      >
        <Icon className="size-3.5" />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-sm leading-snug text-pretty">{item.text}</p>
        <p className="text-muted-foreground mt-0.5 text-xs">
          <span className="text-foreground/70 font-medium">{item.agent}</span>
          {' · '}
          {kindLabel[item.kind]}
          {' · '}
          {item.time}
        </p>
      </div>
    </li>
  )
}
