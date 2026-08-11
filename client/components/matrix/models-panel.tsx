'use client'

/**
 * ModelsPanel — pick the default model agents run on, and browse the catalog.
 * The search picker (SearchSelectDialog, via ModelSelector) is also available
 * here for fast switching. Controlled by `value` + `onChange`.
 */

import { Check, Gauge, Zap, Coins, Lock, Workflow, Cpu, Database } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/matrix/astryx-card'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ModelSelector } from '@/components/matrix/model-selector'
import { cn } from '@/lib/utils'
import { models, type Model } from '@/lib/matrix-data'
import { useSettings } from '@/hooks/api/useSettings'
import { friendlyModel } from '@/lib/api/models'

/** Shown when /settings is unreachable so the section is never empty — the
 *  documented Centra AI default registry, tagged "default" rather than "live". */
const FALLBACK_PINS = { compiler_model: 'gpt-oss-20b', executor_model: 'gpt-oss-120b' }

function Tier({ level, label, icon: Icon }: { level: number; label: string; icon: typeof Zap }) {
  return (
    <span className="text-muted-foreground flex items-center gap-1.5 text-xs" title={label}>
      <Icon className="size-3.5" />
      <span className="flex items-end gap-0.5" aria-hidden="true">
        {[1, 2, 3].map((i) => (
          <span
            key={i}
            className={cn(
              'w-1 rounded-full',
              i === 1 ? 'h-2' : i === 2 ? 'h-3' : 'h-4',
              i <= level ? 'bg-primary' : 'bg-muted/50',
            )}
          />
        ))}
      </span>
      <span className="sr-only">{`${label}: ${level} of 3`}</span>
    </span>
  )
}

export function ModelsPanel({
  value,
  onChange,
}: {
  value: string
  onChange: (id: string) => void
}) {
  const t = useTranslations('modelsPanel')

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PoweringSection />

      <Card>
        <CardContent className="flex flex-wrap items-center justify-between gap-4 p-5">
          <div>
            <p className="text-muted-foreground text-sm">{t('defaultModel')}</p>
            <p className="text-foreground text-base font-medium">{t('defaultModelDesc')}</p>
          </div>
          <ModelSelector value={value} onChange={onChange} />
        </CardContent>
      </Card>

      <div className="flex flex-col gap-3">
        {models.map((m) => (
          <ModelRow
            key={m.id}
            model={m}
            selected={m.id === value}
            onSelect={() => m.available && onChange(m.id)}
          />
        ))}
      </div>
    </div>
  )
}

function PoweringSection() {
  const t = useTranslations('modelsPanel')
  const s = useSettings()
  const live = Boolean(s.data)
  const cfg = s.data ?? FALLBACK_PINS

  const roles = (
    [
      {
        key: 'compiler',
        raw: cfg.compiler_model,
        label: t('rolePlanner'),
        desc: t('rolePlannerDesc'),
        icon: Workflow,
      },
      {
        key: 'executor',
        raw: cfg.executor_model,
        label: t('roleWorker'),
        desc: t('roleWorkerDesc'),
        icon: Cpu,
      },
      {
        key: 'embedder',
        raw: 'embedder_model' in cfg ? cfg.embedder_model : undefined,
        label: t('roleMemory'),
        desc: t('roleMemoryDesc'),
        icon: Database,
      },
    ] as const
  ).filter((r) => Boolean(r.raw))

  return (
    <Card>
      <CardContent className="flex flex-col gap-4 p-5">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="text-foreground text-base font-medium">{t('poweringTitle')}</p>
            <p className="text-muted-foreground text-sm">{t('poweringDesc')}</p>
          </div>
          <span
            className={cn(
              'inline-flex shrink-0 items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium',
              live ? 'bg-primary/12 text-primary' : 'bg-muted text-muted-foreground',
            )}
          >
            <span
              className={cn('size-1.5 rounded-full', live ? 'bg-primary' : 'bg-muted-foreground')}
            />
            {live ? t('live') : t('usingDefaults')}
          </span>
        </div>

        {s.isLoading ? (
          <div className="flex flex-col gap-2">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : (
          <div className="flex flex-col gap-2">
            {roles.map((r) => {
              const fm = friendlyModel(r.raw)
              return (
                <div
                  key={r.key}
                  className="border-border flex items-center gap-3 rounded-xl border p-3"
                  title={r.raw ?? undefined}
                >
                  <span className="bg-primary/10 text-primary flex size-9 shrink-0 items-center justify-center rounded-lg">
                    <r.icon className="size-4" />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-baseline gap-x-2">
                      <p className="text-foreground text-sm font-medium">{r.label}</p>
                      <p className="text-muted-foreground text-xs">{r.desc}</p>
                    </div>
                    <p className="text-foreground mt-0.5 truncate text-sm">
                      {fm?.name}
                      <span className="text-muted-foreground"> · {fm?.provider}</span>
                    </p>
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function ModelRow({
  model,
  selected,
  onSelect,
}: {
  model: Model
  selected: boolean
  onSelect: () => void
}) {
  const t = useTranslations('modelsPanel')

  return (
    <Card
      className={cn(
        'transition-colors',
        selected && 'border-primary/50 ring-primary/30 ring-1',
        !model.available && 'opacity-70',
      )}
    >
      <CardContent className="flex flex-col gap-4 p-5 sm:flex-row sm:items-center">
        <span className="bg-muted text-foreground flex size-11 shrink-0 items-center justify-center rounded-xl text-sm font-semibold">
          {model.name.slice(0, 1)}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-foreground text-sm font-medium">{model.name}</p>
            <span className="text-muted-foreground font-mono text-xs">{model.provider}</span>
            {selected && (
              <span className="bg-primary/12 text-primary inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium">
                <Check className="size-3" />
                {t('default')}
              </span>
            )}
          </div>
          <p className="text-muted-foreground mt-0.5 text-sm text-pretty">{model.blurb}</p>
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1">
            <Tier level={model.speedTier} label={t('speed')} icon={Zap} />
            <Tier level={4 - model.costTier} label={t('value')} icon={Coins} />
            <span className="text-muted-foreground flex items-center gap-1.5 text-xs">
              <Gauge className="size-3.5" />
              {model.contextK >= 1000
                ? t('contextM', { m: model.contextK / 1000 })
                : t('contextK', { k: model.contextK })}
            </span>
          </div>
        </div>
        {model.available ? (
          <Button
            size="sm"
            variant={selected ? 'secondary' : 'outline'}
            onClick={onSelect}
            disabled={selected}
            className="shrink-0"
          >
            {selected ? t('selected') : t('useModel')}
          </Button>
        ) : (
          <span className="text-muted-foreground flex shrink-0 items-center gap-1.5 text-xs">
            <Lock className="size-3.5" />
            {t('byoRuntime')}
          </span>
        )}
      </CardContent>
    </Card>
  )
}
