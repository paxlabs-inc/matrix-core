'use client'

/**
 * AutomatrixSection — the Settings entry for Neo's proactive "surprise tasks".
 *
 * Renders inside SettingsSheet: a master opt-in toggle with a plain-language
 * explanation, plus a "Review" dialog that surfaces the pending opportunity
 * queue (dismiss any item; approve a financial one for a normal, gated run)
 * and the completion inbox with its unread badge.
 *
 * House rules honoured: separation comes from background-color contrast
 * (bg-card panels), never border strokes for depth; no emojis, gradients, or
 * glow. Copy is result-not-protocol — no Chronos/alarm/marker/cortex jargon.
 *
 * The whole section hides itself when the daemon reports no Automatrix control
 * (older daemons without the feature wired).
 */
import { useState } from 'react'
import { useTranslations } from 'next-intl'
import { toast } from 'sonner'
import { Card } from '@astryxdesign/core/Card'
import { Sparkles, Trash2Icon, Check, Bell, ShieldCheck } from '@/lib/matrix-icons'
import { Switch } from '@/components/matrix/astryx-switch'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'
import { Tab, TabList } from '@astryxdesign/core/TabList'
import { Skeleton } from '@/components/ui/skeleton'
import {
  useApproveOpportunity,
  useAutomatrixInbox,
  useAutomatrixQueue,
  useAutomatrixSettings,
  useDismissOpportunity,
  useMarkInboxRead,
  useSetAutomatrixEnabled,
} from '@/hooks/api/useAutomatrix'
import type { AutomatrixInboxItem, AutomatrixQueueItem } from '@/lib/api/automatrix'

type T = ReturnType<typeof useTranslations>

export function AutomatrixSection() {
  const t = useTranslations('automatrix')
  const settings = useAutomatrixSettings()
  const setEnabled = useSetAutomatrixEnabled()
  const [reviewOpen, setReviewOpen] = useState(false)

  // Fetch the inbox whenever the section is available so the unread badge is
  // live; the queue only when the review dialog is open.
  const available = settings.data != null
  const inbox = useAutomatrixInbox(available)
  const unread = inbox.data?.unread ?? 0

  // Hidden entirely when the daemon has no Automatrix control wired.
  if (settings.isSuccess && settings.data === null) return null

  const enabled = settings.data?.enabled ?? false

  return (
    <section className="space-y-3">
      <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
        {t('title')}
      </h3>

      <Card variant="muted" padding={4} width="100%" className="space-y-3">
        <label className="flex cursor-pointer items-start gap-3">
          <Sparkles className="text-primary mt-0.5 size-4 shrink-0" />
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-medium">{t('toggle')}</p>
            <p className="text-muted-foreground text-xs">{t('explanation')}</p>
          </div>
          <Switch
            checked={enabled}
            onCheckedChange={(v) =>
              setEnabled.mutate(v, { onError: () => toast.error(t('toggleError')) })
            }
            disabled={settings.isLoading || setEnabled.isPending}
            aria-label={t('toggle')}
          />
        </label>

        <p className="text-muted-foreground flex items-center gap-1.5 text-xs">
          <ShieldCheck className="text-primary size-3.5 shrink-0" />
          {t('neverMoney')}
        </p>

        <Button
          variant="secondary"
          size="sm"
          className="w-full justify-center"
          onClick={() => setReviewOpen(true)}
        >
          <Bell className="size-4" />
          {t('review')}
          {unread > 0 && (
            <Badge variant="default" className="ml-1">
              {unread}
            </Badge>
          )}
        </Button>
      </Card>

      <ReviewDialog t={t} open={reviewOpen} onOpenChange={setReviewOpen} unread={unread} />
    </section>
  )
}

function ReviewDialog({
  t,
  open,
  onOpenChange,
  unread,
}: {
  t: T
  open: boolean
  onOpenChange: (v: boolean) => void
  unread: number
}) {
  const [tab, setTab] = useState<'queue' | 'inbox'>('queue')
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] gap-0 overflow-hidden p-0 sm:max-w-md">
        <DialogHeader className="p-5 pb-3">
          <DialogTitle>{t('title')}</DialogTitle>
          <DialogDescription>{t('reviewDesc')}</DialogDescription>
        </DialogHeader>
        <div className="min-h-0 px-5 pb-5">
          <TabList
            value={tab}
            onChange={(value) => setTab(value as typeof tab)}
            layout="fill"
            aria-label={t('title')}
          >
            <Tab value="queue" label={t('tabQueue')} />
            <Tab
              value="inbox"
              label={t('tabInbox')}
              endContent={unread > 0 ? <Badge variant="default">{unread}</Badge> : undefined}
            />
          </TabList>
          {tab === 'queue' ? (
            <div className="mt-3 max-h-[55vh] overflow-y-auto">
              <QueueTab t={t} active={open} />
            </div>
          ) : (
            <div className="mt-3 max-h-[55vh] overflow-y-auto">
              <InboxTab t={t} active={open} />
            </div>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}

function QueueTab({ t, active }: { t: T; active: boolean }) {
  const queue = useAutomatrixQueue(active)
  const dismiss = useDismissOpportunity()
  const approve = useApproveOpportunity()

  if (queue.isLoading) return <RowSkeletons />
  const items = queue.data ?? []
  if (items.length === 0) return <EmptyState label={t('queueEmpty')} />

  return (
    <ul className="flex flex-col gap-2">
      {items.map((item) => (
        <QueueRow
          key={item.uri}
          t={t}
          item={item}
          onDismiss={() => dismiss.mutate(item.uri)}
          onApprove={() => approve.mutate(item.uri)}
          busy={dismiss.isPending || approve.isPending}
        />
      ))}
    </ul>
  )
}

function QueueRow({
  t,
  item,
  onDismiss,
  onApprove,
  busy,
}: {
  t: T
  item: AutomatrixQueueItem
  onDismiss: () => void
  onApprove: () => void
  busy: boolean
}) {
  return (
    <li className="bg-card rounded-lg p-3">
      <div className="flex items-start gap-2">
        <p className="text-foreground min-w-0 flex-1 text-sm font-medium [overflow-wrap:anywhere]">
          {item.summary}
        </p>
        {item.financial && (
          <Badge variant="secondary" className="shrink-0">
            {t('financial')}
          </Badge>
        )}
      </div>
      {item.rationale && (
        <p className="text-muted-foreground mt-1 text-xs [overflow-wrap:anywhere]">
          {item.rationale}
        </p>
      )}
      <div className="mt-2.5 flex items-center gap-2">
        {item.financial && (
          <Button size="sm" variant="secondary" onClick={onApprove} disabled={busy}>
            <Check className="size-4" />
            {t('approve')}
          </Button>
        )}
        <Button size="sm" variant="ghost" onClick={onDismiss} disabled={busy}>
          <Trash2Icon className="size-4" />
          {t('dismiss')}
        </Button>
      </div>
    </li>
  )
}

function InboxTab({ t, active }: { t: T; active: boolean }) {
  const inbox = useAutomatrixInbox(active)
  const markRead = useMarkInboxRead()

  if (inbox.isLoading) return <RowSkeletons />
  const items = inbox.data?.items ?? []
  if (items.length === 0) return <EmptyState label={t('inboxEmpty')} />

  return (
    <ul className="flex flex-col gap-2">
      {items.map((item) => (
        <InboxRow
          key={item.id}
          item={item}
          onOpen={() => {
            if (!item.read) markRead.mutate(item.id)
          }}
        />
      ))}
    </ul>
  )
}

function InboxRow({ item, onOpen }: { item: AutomatrixInboxItem; onOpen: () => void }) {
  return (
    <li
      className={item.read ? 'bg-card rounded-lg p-3' : 'bg-accent rounded-lg p-3'}
      onMouseEnter={onOpen}
    >
      <div className="flex items-start gap-2">
        <p className="text-foreground min-w-0 flex-1 text-sm font-medium [overflow-wrap:anywhere]">
          {item.opportunity_summary}
        </p>
        {!item.read && <span className="bg-primary mt-1.5 size-1.5 shrink-0 rounded-full" />}
      </div>
      {item.result_summary && (
        <p className="text-muted-foreground mt-1 text-xs [overflow-wrap:anywhere]">
          {item.result_summary}
        </p>
      )}
      {item.created_at && (
        <p className="text-muted-foreground mt-1.5 text-[11px]">{formatWhen(item.created_at)}</p>
      )}
    </li>
  )
}

function RowSkeletons() {
  return (
    <div className="flex flex-col gap-2">
      {[0, 1, 2].map((i) => (
        <Skeleton key={i} className="h-16 w-full rounded-lg" />
      ))}
    </div>
  )
}

function EmptyState({ label }: { label: string }) {
  return (
    <div className="bg-card text-muted-foreground rounded-lg p-6 text-center text-sm">{label}</div>
  )
}

function formatWhen(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  })
}
