'use client'

import { useTranslations } from 'next-intl'
import { ProgressSteps } from '@/components/tool-ui/progress-tracker/progress-tracker'
import type { ProgressStep } from '@/components/tool-ui/progress-tracker/schema'
import type { RunStage, StageStatus } from '@/lib/matrix-data'

/**
 * RunPipeline — the canonical Plan → Gather → Execute → Verify → Sign steps,
 * now rendered with the Tool UI ProgressSteps timeline (vertical, with live
 * shimmer on the active step and a connecting rail). Backend-agnostic: it
 * renders whatever stages array it is given.
 */
const STAGE_STATUS_MAP: Record<StageStatus, ProgressStep['status']> = {
  done: 'completed',
  active: 'in-progress',
  pending: 'pending',
  failed: 'failed',
}

function toProgressSteps(stages: RunStage[]): ProgressStep[] {
  return stages.map((stage) => ({
    id: stage.id,
    label: stage.label,
    description: stage.detail,
    status: STAGE_STATUS_MAP[stage.status],
  }))
}

export function RunPipeline({
  stages,
  className,
}: {
  stages: RunStage[]
  className?: string
  /** Retained for API compatibility; the timeline always shows labels. */
  showLabels?: boolean
}) {
  const t = useTranslations('runPipeline')

  return (
    <div className={className} role="group" aria-label={t('srLabel')}>
      <ProgressSteps steps={toProgressSteps(stages)} />
    </div>
  )
}
