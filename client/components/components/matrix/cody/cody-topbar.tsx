'use client'

/**
 * Coding-surface top bar — project name and live run status, on Neo's
 * identity. Sits on `bg-background`; the page body below uses `bg-card`, so
 * the two read as separate layers without a border stroke.
 */
import { SidebarTrigger } from '@/components/ui/sidebar'
import { Button } from '@/components/ui/button'
import { IconStop } from '@/components/matrix/cody/icons'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import type { NeoProject } from '@/lib/api/workspace'
import type { ChatPhase } from '@/hooks/api/useChat'

const PHASE_COPY: Record<ChatPhase, string> = {
  idle: '',
  thinking: 'Thinking',
  working: 'Working',
}

export function CodyTopbar({
  project,
  phase,
  onStop,
}: {
  project: NeoProject | null
  phase: ChatPhase
  onStop: () => void
}) {
  const running = phase !== 'idle'
  return (
    <header className="bg-background flex h-14 shrink-0 items-center gap-3 px-3">
      <SidebarTrigger />
      <div className="flex min-w-0 items-center gap-2">
        <span className="truncate text-sm font-medium">{project?.name ?? 'Neo'}</span>
      </div>

      <div className="ml-auto flex items-center gap-3">
        {running ? (
          <div className="flex items-center gap-2">
            <CodyLoader variant="dots" />
            <span className="text-muted-foreground font-mono text-xs" data-run-status={phase}>
              {PHASE_COPY[phase]}
            </span>
          </div>
        ) : null}
        {phase === 'working' ? (
          <Button size="sm" variant="secondary" onClick={onStop}>
            <IconStop className="size-3.5" />
            <span>Stop</span>
          </Button>
        ) : null}
      </div>
    </header>
  )
}
