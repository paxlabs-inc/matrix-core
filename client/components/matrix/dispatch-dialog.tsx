'use client'

import { useState } from 'react'
import { Plus, Send } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { TextArea } from '@astryxdesign/core/TextArea'
import { Selector } from '@astryxdesign/core/Selector'
import { ToggleButton, ToggleButtonGroup } from '@astryxdesign/core/ToggleButton'
import { Text } from '@astryxdesign/core/Text'
import {
  agents as fallbackAgents,
  tools as fallbackTools,
  type Autonomy,
  type Agent,
  type Tool,
} from '@/lib/matrix-data'

export interface DispatchPayload {
  goal: string
  agentId: string
  autonomy: Autonomy
  toolIds: string[]
  /** Backing skill URI of the chosen agent, when sourced from the live
   *  skill catalogue. The daemon compiles the task against this skill. */
  skill?: string
}

export function DispatchDialog({
  trigger,
  onDispatch,
  agents = fallbackAgents,
  tools = fallbackTools,
}: {
  trigger?: React.ReactNode
  onDispatch?: (payload: DispatchPayload) => void
  agents?: Agent[]
  tools?: Tool[]
}) {
  const t = useTranslations('dispatchDialog')
  const [open, setOpen] = useState(false)
  const [goal, setGoal] = useState('')
  const [agentId, setAgentId] = useState(agents[0]?.id ?? fallbackAgents[0].id)
  const [autonomy, setAutonomy] = useState<Autonomy>('checkpoints')
  const [toolIds, setToolIds] = useState<string[]>(
    tools.filter((tool) => tool.enabled).map((tool) => tool.id),
  )

  const canSubmit = goal.trim().length > 8

  function submit() {
    if (!canSubmit) return
    const selected = agents.find((a) => a.id === agentId)
    onDispatch?.({ goal: goal.trim(), agentId, autonomy, toolIds, skill: selected?.skillUri })
    setGoal('')
    setOpen(false)
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {trigger ?? (
          <Button>
            <Plus data-icon="inline-start" />
            {t('trigger')}
          </Button>
        )}
      </DialogTrigger>
      <DialogContent className="max-h-[92dvh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t('title')}</DialogTitle>
          <DialogDescription>{t('description')}</DialogDescription>
        </DialogHeader>

        <div className="space-y-5">
          <TextArea
            label={t('goalLabel')}
            description={t('goalDesc')}
            value={goal}
            onChange={setGoal}
            placeholder={t('goalPlaceholder')}
            rows={5}
            isRequired
            width="100%"
          />

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Selector
              label={t('agentLabel')}
              value={agentId}
              onChange={setAgentId}
              options={agents.map((agent) => ({
                value: agent.id,
                label: `${agent.name} — ${agent.role}`,
              }))}
              width="100%"
            />

            <div>
              <ToggleButtonGroup
                value={autonomy}
                onChange={(value) => value && setAutonomy(value as Autonomy)}
                label={t('autonomyLabel')}
                size="sm"
              >
                <ToggleButton value="supervised" label={t('supervised')} />
                <ToggleButton value="checkpoints" label={t('checkpoints')} />
                <ToggleButton value="full" label={t('full')} />
              </ToggleButtonGroup>
            </div>
          </div>

          <div className="space-y-2">
            <Text type="label" weight="bold" display="block">
              {t('toolsLabel')}
            </Text>
            <div className="flex flex-wrap gap-2">
              {tools.map((tool) => {
                const on = toolIds.includes(tool.id)
                return (
                  <ToggleButton
                    key={tool.id}
                    label={tool.name}
                    isPressed={on}
                    onPressedChange={(next) =>
                      setToolIds((prev) =>
                        next ? [...prev, tool.id] : prev.filter((id) => id !== tool.id),
                      )
                    }
                    size="sm"
                  />
                )
              })}
            </div>
            <Text type="supporting" color="secondary" display="block">
              {t('toolsDesc')}
            </Text>
          </div>
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => setOpen(false)}>
            {t('cancel')}
          </Button>
          <Button onClick={submit} disabled={!canSubmit}>
            <Send data-icon="inline-start" />
            {t('submit')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
