'use client'

import { MoreHorizontal, Pause, Play, ArrowRight, RotateCcw } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/matrix/astryx-card'
import { ProgressBar } from '@astryxdesign/core/ProgressBar'
import { DropdownMenu, DropdownMenuItem } from '@astryxdesign/core/DropdownMenu'
import { StatusPill } from '@/components/matrix/status-pill'
import { RunPipeline } from '@/components/matrix/run-pipeline'
import { AUTONOMY_LABEL, getAgent, getTool, relativeTime, type Run } from '@/lib/matrix-data'

export function RunCard({
  run,
  onViewReceipt,
  onPauseResume,
  onRetry,
  onOpenTranscript,
  showStageLabels = false,
}: {
  run: Run
  onViewReceipt?: (run: Run) => void
  onPauseResume?: (run: Run) => void
  onRetry?: (run: Run) => void
  onOpenTranscript?: (run: Run) => void
  showStageLabels?: boolean
}) {
  const t = useTranslations('runCard')
  const agent = getAgent(run.agentId)
  const isDone = run.status === 'completed'
  const isFailed = run.status === 'failed'
  const isRunning = run.status === 'running'

  return (
    <Card className="min-w-0">
      <CardContent className="flex flex-col gap-4 p-5">
        <div className="flex items-start justify-between gap-3">
          <div className="flex min-w-0 flex-col gap-1.5">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <StatusPill status={run.status} />
              <span className="text-muted-foreground min-w-0 truncate font-mono text-xs">
                {run.id}
              </span>
            </div>
            <h3 className="text-foreground text-sm leading-snug font-medium text-pretty">
              {run.title}
            </h3>
          </div>

          <DropdownMenu
            button={{
              label: t('srActions'),
              icon: <MoreHorizontal />,
              variant: 'ghost',
              size: 'sm',
              isIconOnly: true,
            }}
            placement="below"
            menuWidth={192}
          >
            {isDone && (
              <DropdownMenuItem
                label={t('viewReceipt')}
                icon={<ArrowRight />}
                onClick={() => onViewReceipt?.(run)}
              />
            )}
            {(isRunning || run.status === 'paused') && (
              <DropdownMenuItem
                label={isRunning ? t('pauseRun') : t('resumeRun')}
                icon={isRunning ? <Pause /> : <Play />}
                onClick={() => onPauseResume?.(run)}
              />
            )}
            {isFailed && (
              <DropdownMenuItem
                label={t('retryRun')}
                icon={<RotateCcw />}
                onClick={() => onRetry?.(run)}
              />
            )}
            <DropdownMenuItem label={t('openTranscript')} onClick={() => onOpenTranscript?.(run)} />
            <DropdownMenuItem label={t('cancel')} />
          </DropdownMenu>
        </div>

        <RunPipeline stages={run.stages} showLabels={showStageLabels} />

        {!isDone && !isFailed && (
          <div className="flex flex-col gap-1.5">
            <ProgressBar
              value={run.progress}
              label={t('progress', { progress: run.progress })}
              isLabelHidden
            />
            <span className="text-muted-foreground font-mono text-xs">
              {t('progress', { progress: run.progress })}
            </span>
          </div>
        )}

        <div className="text-muted-foreground flex flex-wrap items-center gap-x-4 gap-y-1.5 pt-3 text-xs">
          <span>
            {t('agent')} <span className="text-foreground font-medium">{agent?.name ?? '—'}</span>
          </span>
          <span className="font-mono">{AUTONOMY_LABEL[run.autonomy]}</span>
          <span className="flex min-w-0 flex-wrap items-center gap-1.5">
            {run.toolIds.map((id) => (
              <span
                key={id}
                className="border-border bg-muted rounded border px-1.5 py-0.5 font-mono text-[10px]"
              >
                {getTool(id)?.name ?? id}
              </span>
            ))}
          </span>
          <span className="ml-auto font-mono">{relativeTime(run.createdAt)}</span>
        </div>
      </CardContent>
    </Card>
  )
}
