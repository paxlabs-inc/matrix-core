'use client'

/**
 * BriefSection — the Settings entry for the personalized morning brief.
 *
 * Renders inside SettingsSheet: a master opt-in toggle (default off), the
 * schedule (delivery time, timezone, days, length, sections), pause without
 * losing preferences, the guided-interview entry (start/repeat — the interview
 * itself runs in the normal chat surface), and the saved profile controls
 * (view / export / delete).
 *
 * House rules honoured: separation comes from background-color contrast
 * (bg-card panels), never border strokes for depth; no emojis, gradients, or
 * glow. Copy is result-not-protocol.
 *
 * The whole section hides itself when the daemon reports no brief control
 * (older daemons without the feature wired).
 */
import { useState } from 'react'
import { useLocale, useTranslations } from 'next-intl'
import { toast } from 'sonner'
import { Clock, Download, FileText, MessageSquare, Sparkles, Trash2Icon } from '@/lib/matrix-icons'
import { Switch } from '@/components/ui/switch'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'
import {
  useBriefSettings,
  useDeletePersonalization,
  useExportPersonalization,
  usePersonalization,
  useStartInterview,
  useUpdateBriefSettings,
} from '@/hooks/api/useBrief'
import { BRIEF_LENGTHS, BRIEF_SECTIONS } from '@/lib/api/brief'

type T = ReturnType<typeof useTranslations>

/** Narrow weekday labels (Sun..Sat) in the active locale. */
function weekdayLabels(locale: string): string[] {
  const fmt = new Intl.DateTimeFormat(locale, { weekday: 'narrow' })
  // 2026-02-01 is a Sunday; walk one week.
  return Array.from({ length: 7 }, (_, i) => fmt.format(new Date(Date.UTC(2026, 1, 1 + i))))
}

export function BriefSection({
  onOpenConversation,
}: {
  /** Opens a conversation in the chat surface (closes settings first). */
  onOpenConversation?: (id: string) => void
}) {
  const t = useTranslations('brief')
  const locale = useLocale()
  const settings = useBriefSettings()
  const update = useUpdateBriefSettings()
  const interview = useStartInterview()
  const [profileOpen, setProfileOpen] = useState(false)

  // Hidden entirely when the daemon has no brief control wired.
  if (settings.isSuccess && settings.data === null) return null

  const st = settings.data
  const enabled = st?.enabled ?? false
  const days = st?.days ?? []
  const sections = st?.sections ?? []
  const dayNames = weekdayLabels(locale)

  const apply = (u: Parameters<typeof update.mutate>[0]) =>
    update.mutate(u, { onError: () => toast.error(t('updateError')) })

  const toggleDay = (d: number) =>
    apply({ days: days.includes(d) ? days.filter((x) => x !== d) : [...days, d].sort() })

  const toggleSection = (s: string) =>
    apply({
      sections: sections.includes(s) ? sections.filter((x) => x !== s) : [...sections, s],
    })

  const startInterviewFlow = () =>
    interview.mutate(undefined, {
      onSuccess: ({ conversation_id }) => onOpenConversation?.(conversation_id),
      onError: () => toast.error(t('interviewError')),
    })

  return (
    <section className="space-y-3">
      <h3 className="text-muted-foreground text-xs font-medium tracking-wide uppercase">
        {t('title')}
      </h3>

      <div className="bg-card space-y-4 rounded-lg p-4">
        <label className="flex cursor-pointer items-start gap-3">
          <Sparkles className="text-primary mt-0.5 size-4 shrink-0" />
          <div className="min-w-0 flex-1">
            <p className="text-foreground text-sm font-medium">{t('toggle')}</p>
            <p className="text-muted-foreground text-xs">{t('explanation')}</p>
          </div>
          <Switch
            checked={enabled}
            onCheckedChange={(v) =>
              apply(
                v
                  ? {
                      enabled: true,
                      timezone: st?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone,
                      delivery_time: st?.delivery_time || '07:30',
                    }
                  : { enabled: false },
              )
            }
            disabled={settings.isLoading || update.isPending}
            aria-label={t('toggle')}
          />
        </label>

        {enabled && (
          <div className="space-y-4">
            {/* Delivery time + pause */}
            <div className="flex flex-wrap items-center gap-3">
              <label className="flex items-center gap-2">
                <Clock className="text-muted-foreground size-4" />
                <span className="text-foreground text-sm">{t('time')}</span>
                <input
                  type="time"
                  value={st?.delivery_time ?? '07:30'}
                  onChange={(e) => e.target.value && apply({ delivery_time: e.target.value })}
                  className="bg-background text-foreground rounded-md px-2 py-1 text-sm"
                  aria-label={t('time')}
                />
              </label>
              <label className="ml-auto flex items-center gap-2">
                <span className="text-muted-foreground text-xs">{t('pause')}</span>
                <Switch
                  checked={st?.paused ?? false}
                  onCheckedChange={(v) => apply({ paused: v })}
                  aria-label={t('pause')}
                />
              </label>
            </div>

            {/* Days */}
            <div>
              <p className="text-muted-foreground mb-1.5 text-xs">{t('days')}</p>
              <div className="flex gap-1.5">
                {dayNames.map((name, d) => {
                  const on = days.length === 0 || days.includes(d)
                  return (
                    <button
                      key={d}
                      type="button"
                      onClick={() => toggleDay(d)}
                      aria-pressed={on}
                      className={
                        on
                          ? 'bg-primary text-primary-foreground size-7 rounded-full text-xs font-medium'
                          : 'bg-background text-muted-foreground size-7 rounded-full text-xs'
                      }
                    >
                      {name}
                    </button>
                  )
                })}
              </div>
              <p className="text-muted-foreground mt-1 text-[11px]">{t('daysHint')}</p>
            </div>

            {/* Length */}
            <div>
              <p className="text-muted-foreground mb-1.5 text-xs">{t('length')}</p>
              <div className="flex gap-1.5">
                {BRIEF_LENGTHS.map((l) => (
                  <button
                    key={l}
                    type="button"
                    onClick={() => apply({ length: l })}
                    aria-pressed={(st?.length ?? 'standard') === l}
                    className={
                      (st?.length ?? 'standard') === l
                        ? 'bg-primary text-primary-foreground rounded-full px-3 py-1 text-xs font-medium'
                        : 'bg-background text-muted-foreground rounded-full px-3 py-1 text-xs'
                    }
                  >
                    {t(`length_${l}`)}
                  </button>
                ))}
              </div>
            </div>

            {/* Sections */}
            <div>
              <p className="text-muted-foreground mb-1.5 text-xs">{t('sections')}</p>
              <div className="flex flex-wrap gap-1.5">
                {BRIEF_SECTIONS.map((s) => {
                  const on = sections.includes(s)
                  return (
                    <button
                      key={s}
                      type="button"
                      onClick={() => toggleSection(s)}
                      aria-pressed={on}
                      className={
                        on
                          ? 'bg-primary text-primary-foreground rounded-full px-3 py-1 text-xs font-medium'
                          : 'bg-background text-muted-foreground rounded-full px-3 py-1 text-xs'
                      }
                    >
                      {t(`section_${s}`)}
                    </button>
                  )
                })}
              </div>
            </div>
          </div>
        )}

        {/* Interview + profile controls */}
        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={startInterviewFlow}
            disabled={interview.isPending}
          >
            <MessageSquare className="size-4" />
            {t('interview')}
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setProfileOpen(true)}>
            <FileText className="size-4" />
            {t('viewProfile')}
          </Button>
        </div>
        <p className="text-muted-foreground text-xs">{t('interviewHint')}</p>
      </div>

      <ProfileDialog t={t} open={profileOpen} onOpenChange={setProfileOpen} />
    </section>
  )
}

function ProfileDialog({
  t,
  open,
  onOpenChange,
}: {
  t: T
  open: boolean
  onOpenChange: (v: boolean) => void
}) {
  const profile = usePersonalization(open)
  const exportProfile = useExportPersonalization()
  const deleteProfile = useDeletePersonalization()
  const [confirming, setConfirming] = useState(false)

  const p = profile.data?.profile
  const lists: { label: string; values: string[] }[] = []
  if (p) {
    if (p.interests?.length) lists.push({ label: t('profileInterests'), values: p.interests })
    if (p.day_to_day_goals?.length)
      lists.push({ label: t('profileGoals'), values: p.day_to_day_goals })
    for (const [cat, taste] of Object.entries(p.media ?? {})) {
      if (taste.liked?.length)
        lists.push({ label: t('profileLikes', { category: cat }), values: taste.liked })
      if (taste.disliked?.length)
        lists.push({ label: t('profileDislikes', { category: cat }), values: taste.disliked })
    }
    if (p.adventurousness)
      lists.push({ label: t('profileAdventurousness'), values: [p.adventurousness] })
  }
  const empty = lists.length === 0

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t('profileTitle')}</DialogTitle>
          <DialogDescription>{t('profileDesc')}</DialogDescription>
        </DialogHeader>

        {empty ? (
          <div className="bg-card text-muted-foreground rounded-lg p-6 text-center text-sm">
            {t('profileEmpty')}
          </div>
        ) : (
          <div className="space-y-3">
            {lists.map((row) => (
              <div key={row.label} className="bg-card rounded-lg p-3">
                <p className="text-muted-foreground text-xs">{row.label}</p>
                <p className="text-foreground mt-0.5 text-sm">{row.values.join(', ')}</p>
              </div>
            ))}
          </div>
        )}

        <div className="flex items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() =>
              exportProfile.mutate(undefined, {
                onError: () => toast.error(t('exportError')),
              })
            }
            disabled={exportProfile.isPending || empty}
          >
            <Download className="size-4" />
            {t('export')}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            className="text-destructive"
            onClick={() => {
              if (!confirming) {
                setConfirming(true)
                return
              }
              deleteProfile.mutate(undefined, {
                onSuccess: () => {
                  setConfirming(false)
                  toast.success(t('deleted'))
                },
                onError: () => toast.error(t('deleteError')),
              })
            }}
            disabled={deleteProfile.isPending || empty}
          >
            <Trash2Icon className="size-4" />
            {confirming ? t('deleteConfirm') : t('delete')}
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
