'use client'

/**
 * Cody top bar — project name, mode chip, live run status, and stop. Sits on
 * `bg-background`; the page body below uses `bg-card`, so the two read as
 * separate layers without a border stroke.
 */
import { SidebarTrigger } from '@/components/ui/sidebar'
import { Button } from '@/components/ui/button'
import { IconStop } from '@/components/matrix/cody/icons'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { MODE_LABELS } from '@/components/matrix/cody/tiering'
import type { CodyProject } from '@/lib/api/cody'
import type { CodyRun } from '@/hooks/api/useCody'

const STATUS_COPY: Record<string, string> = {
  running: 'Working',
  needs_input: 'Needs input',
  completed: 'Done',
  failed: 'Failed',
  stopped: 'Stopped',
}

export function CodyTopbar({
  project,
  run,
  isLive,
  onStop,
}: {
  project: CodyProject | null
  run: CodyRun | null
  isLive: boolean
  onStop: () => void
}) {
  const status = run?.status
  const running = status === 'running'
  const canStop = isLive && (running || status === 'needs_input')

  return (
    <header className="bg-background flex h-14 shrink-0 items-center gap-3 px-3">
      <SidebarTrigger />
      <div className="flex min-w-0 items-center gap-2">
        <span className="truncate text-sm font-medium">{project?.name ?? 'Cody'}</span>
        {project ? (
          <span className="bg-surface-secondary text-muted-foreground rounded-md px-2 py-0.5 font-mono text-[10px] tracking-wide uppercase">
            {MODE_LABELS[project.mode]}
          </span>
        ) : null}
      </div>

      <div className="ml-auto flex items-center gap-3">
        {status ? (
          <div className="flex items-center gap-2">
            {running ? <CodyLoader variant="dots" /> : null}
            <span className="text-muted-foreground font-mono text-xs" data-run-status={status}>
              {STATUS_COPY[status] ?? status}
            </span>
          </div>
        ) : null}
        {canStop ? (
          <Button size="sm" variant="secondary" onClick={onStop}>
            <IconStop className="size-3.5" />
            <span>Stop</span>
          </Button>
        ) : null}
      </div>
    </header>
  )
}
