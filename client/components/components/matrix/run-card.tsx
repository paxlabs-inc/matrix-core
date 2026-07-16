'use client'

import { MoreHorizontal, Pause, Play, ArrowRight, RotateCcw } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Progress } from '@/components/ui/progress'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
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

          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon" className="size-8 shrink-0">
                <MoreHorizontal />
                <span className="sr-only">{t('srActions')}</span>
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuGroup>
                {isDone && (
                  <DropdownMenuItem onClick={() => onViewReceipt?.(run)}>
                    <ArrowRight />
                    {t('viewReceipt')}
                  </DropdownMenuItem>
                )}
                {(isRunning || run.status === 'paused') && (
                  <DropdownMenuItem onClick={() => onPauseResume?.(run)}>
                    {isRunning ? <Pause /> : <Play />}
                    {isRunning ? t('pauseRun') : t('resumeRun')}
                  </DropdownMenuItem>
                )}
                {isFailed && (
                  <DropdownMenuItem onClick={() => onRetry?.(run)}>
                    <RotateCcw />
                    {t('retryRun')}
                  </DropdownMenuItem>
                )}
              </DropdownMenuGroup>
              <DropdownMenuSeparator />
              <DropdownMenuGroup>
                <DropdownMenuItem onClick={() => onOpenTranscript?.(run)}>
                  {t('openTranscript')}
                </DropdownMenuItem>
                <DropdownMenuItem variant="destructive">{t('cancel')}</DropdownMenuItem>
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>

        <RunPipeline stages={run.stages} showLabels={showStageLabels} />

        {!isDone && !isFailed && (
          <div className="flex flex-col gap-1.5">
            <Progress value={run.progress} className="h-1.5" />
            <span className="text-muted-foreground font-mono text-xs">
              {t('progress', { progress: run.progress })}
            </span>
          </div>
        )}

        <div className="border-border text-muted-foreground flex flex-wrap items-center gap-x-4 gap-y-1.5 border-t pt-3 text-xs">
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
