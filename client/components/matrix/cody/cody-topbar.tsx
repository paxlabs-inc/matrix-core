'use client'

/**
 * Coding-surface top bar — project name and live run status, on Neo's
 * identity. Sits on `bg-background`; the page body below uses `bg-card`, so
 * the two read as separate layers without a border stroke.
 */
import { Button } from '@astryxdesign/core/Button'
import { Toolbar } from '@astryxdesign/core/Toolbar'
import { Text } from '@astryxdesign/core/Text'
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
    <Toolbar
      label="Coding workspace"
      size="md"
      variant="transparent"
      className="bg-background h-14 shrink-0 px-3"
      startContent={
        <Text type="label" maxLines={1}>
          {project?.name ?? 'Neo'}
        </Text>
      }
      endContent={
        <>
          {running ? (
            <div className="flex items-center gap-2">
              <CodyLoader variant="dots" />
              <Text type="code" color="secondary" data-run-status={phase}>
                {PHASE_COPY[phase]}
              </Text>
            </div>
          ) : null}
          {phase === 'working' ? (
            <Button
              label="Stop"
              size="sm"
              variant="secondary"
              icon={<IconStop className="size-3.5" />}
              onClick={onStop}
            />
          ) : null}
        </>
      }
    />
  )
}
