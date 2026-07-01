'use client'

/**
 * NotificationsPanel — the bell in the top bar. Opens a popover listing the
 * live notification feed (derived from runs + receipts via useNotifications)
 * with an unread badge. Opening the panel marks everything read after a beat
 * so the unread styling is briefly visible.
 */
import { useState } from 'react'
import { Bell, CheckCheck, CircleAlert, CircleCheck, CircleX } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { cn } from '@/lib/utils'
import { relativeTime } from '@/lib/matrix-data'
import type { Notification, NotifKind } from '@/lib/notifications'

const KIND_META: Record<NotifKind, { icon: typeof Bell; tone: string; labelKey: string }> = {
  completed: { icon: CircleCheck, tone: 'text-primary', labelKey: 'completed' },
  needs_input: { icon: CircleAlert, tone: 'text-foreground', labelKey: 'needsInput' },
  failed: { icon: CircleX, tone: 'text-destructive', labelKey: 'failed' },
}

export function NotificationsPanel({
  items,
  unreadCount,
  readIds,
  onMarkAllRead,
}: {
  items: Notification[]
  unreadCount: number
  readIds: Set<string>
  onMarkAllRead: () => void
}) {
  const t = useTranslations('notifications')
  const [open, setOpen] = useState(false)

  function handleOpenChange(next: boolean) {
    setOpen(next)
    if (next && unreadCount > 0) {
      // Let the unread styling register, then clear the badge.
      window.setTimeout(onMarkAllRead, 1500)
    }
  }

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="icon" className="relative size-9" aria-label={t('title')}>
          <Bell />
          {unreadCount > 0 && (
            <span className="bg-primary text-primary-foreground absolute -top-0.5 -right-0.5 flex min-w-4 items-center justify-center rounded-full px-1 text-[10px] leading-4 font-semibold">
              {unreadCount > 9 ? '9+' : unreadCount}
            </span>
          )}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 p-0 sm:w-96">
        <div className="border-border flex items-center justify-between gap-2 border-b px-4 py-3">
          <p className="text-foreground text-sm font-medium">{t('title')}</p>
          {items.length > 0 && unreadCount > 0 && (
            <button
              type="button"
              onClick={onMarkAllRead}
              className="text-muted-foreground hover:text-foreground inline-flex items-center gap-1 text-xs transition-colors"
            >
              <CheckCheck className="size-3.5" />
              {t('markAllRead')}
            </button>
          )}
        </div>

        {items.length === 0 ? (
          <div className="flex flex-col items-center gap-1 px-6 py-10 text-center">
            <Bell className="text-muted-foreground size-5" />
            <p className="text-foreground text-sm font-medium">{t('emptyTitle')}</p>
            <p className="text-muted-foreground text-xs">{t('emptyDesc')}</p>
          </div>
        ) : (
          <ul className="divide-border max-h-96 divide-y overflow-y-auto">
            {items.map((n) => {
              const meta = KIND_META[n.kind]
              const unread = !readIds.has(n.id)
              return (
                <li
                  key={n.id}
                  className={cn('flex items-start gap-3 px-4 py-3', unread && 'bg-primary/[0.04]')}
                >
                  <meta.icon className={cn('mt-0.5 size-4 shrink-0', meta.tone)} />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <p className="text-foreground text-sm font-medium">{t(meta.labelKey)}</p>
                      {unread && <span className="bg-primary size-1.5 shrink-0 rounded-full" />}
                    </div>
                    <p className="text-muted-foreground truncate text-xs">{n.subject}</p>
                    <p className="text-muted-foreground/70 mt-0.5 text-[11px]">
                      {relativeTime(n.at)}
                    </p>
                  </div>
                </li>
              )
            })}
          </ul>
        )}
      </PopoverContent>
    </Popover>
  )
}
