'use client'

/**
 * ModelSelector — a compact pill that opens the searchable model picker
 * (SearchSelectDialog). Controlled: pass `value` + `onChange`. The picker
 * dialog can also be opened externally via the `open`/`onOpenChange` props
 * (used by the control dock), or internally by clicking the pill.
 */

import { useState } from 'react'
import { ChevronDown, Cpu } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'
import { SearchSelectDialog, type SelectOption } from '@/components/matrix/search-select-dialog'
import { models, getModel } from '@/lib/matrix-data'

function tierBars(level: number) {
  return (
    <span className="inline-flex items-end gap-0.5" aria-hidden="true">
      {[1, 2, 3].map((i) => (
        <span
          key={i}
          className={cn(
            'w-0.5 rounded-full',
            i === 1 ? 'h-1.5' : i === 2 ? 'h-2.5' : 'h-3.5',
            i <= level ? 'bg-primary' : 'bg-muted/50',
          )}
        />
      ))}
    </span>
  )
}

function toOptions(): SelectOption[] {
  return models.map((m) => ({
    id: m.id,
    label: m.name,
    description: m.blurb,
    keywords: m.provider,
    disabled: !m.available,
    leading: (
      <span className="bg-muted text-foreground flex size-8 items-center justify-center rounded-lg text-xs font-semibold">
        {m.name.slice(0, 1)}
      </span>
    ),
    meta: (
      <span className="flex items-center gap-2">
        <span className="hidden items-center gap-1 sm:flex" title="Speed">
          {tierBars(m.speedTier)}
        </span>
        <span className="font-mono">
          {m.contextK >= 1000 ? `${m.contextK / 1000}M` : `${m.contextK}K`}
        </span>
      </span>
    ),
  }))
}

export function ModelSelector({
  value,
  onChange,
  open,
  onOpenChange,
  variant = 'pill',
  className,
}: {
  value: string
  onChange: (id: string) => void
  open?: boolean
  onOpenChange?: (open: boolean) => void
  variant?: 'pill' | 'hidden'
  className?: string
}) {
  const t = useTranslations('modelSelector')
  const [internalOpen, setInternalOpen] = useState(false)
  const isOpen = open ?? internalOpen
  const setOpen = onOpenChange ?? setInternalOpen
  const current = getModel(value)

  return (
    <>
      {variant === 'pill' && (
        <button
          type="button"
          onClick={() => setOpen(true)}
          className={cn(
            'border-border bg-card text-foreground hover:bg-accent flex h-9 items-center gap-2 rounded-full border px-3 text-sm font-medium transition-colors',
            className,
          )}
        >
          <Cpu className="text-primary size-4" />
          <span className="max-w-[8rem] truncate">{current?.name ?? t('selectModel')}</span>
          <ChevronDown className="text-muted-foreground size-4" />
        </button>
      )}

      <SearchSelectDialog
        open={isOpen}
        onOpenChange={setOpen}
        title={t('dialogTitle')}
        placeholder={t('searchPlaceholder')}
        options={toOptions()}
        selectedId={value}
        onSelect={onChange}
        emptyLabel={t('emptyLabel')}
      />
    </>
  )
}
