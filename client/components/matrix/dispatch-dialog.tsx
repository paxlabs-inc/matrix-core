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
import { Textarea } from '@/components/ui/textarea'
import { Label } from '@/components/ui/label'
import { Field, FieldGroup, FieldLabel, FieldDescription } from '@/components/ui/field'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { Toggle } from '@/components/ui/toggle'
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

        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="goal">{t('goalLabel')}</FieldLabel>
            <Textarea
              id="goal"
              value={goal}
              onChange={(e) => setGoal(e.target.value)}
              placeholder={t('goalPlaceholder')}
              className="min-h-24 resize-none"
            />
            <FieldDescription>{t('goalDesc')}</FieldDescription>
          </Field>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field>
              <FieldLabel htmlFor="agent">{t('agentLabel')}</FieldLabel>
              <Select value={agentId} onValueChange={setAgentId}>
                <SelectTrigger id="agent">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    {agents.map((a) => (
                      <SelectItem key={a.id} value={a.id}>
                        {a.name} — {a.role}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
            </Field>

            <Field>
              <FieldLabel>{t('autonomyLabel')}</FieldLabel>
              <ToggleGroup
                type="single"
                value={autonomy}
                onValueChange={(v) => v && setAutonomy(v as Autonomy)}
                variant="outline"
                className="w-full"
              >
                <ToggleGroupItem value="supervised" className="flex-1 text-xs">
                  {t('supervised')}
                </ToggleGroupItem>
                <ToggleGroupItem value="checkpoints" className="flex-1 text-xs">
                  {t('checkpoints')}
                </ToggleGroupItem>
                <ToggleGroupItem value="full" className="flex-1 text-xs">
                  {t('full')}
                </ToggleGroupItem>
              </ToggleGroup>
            </Field>
          </div>

          <Field>
            <Label className="text-sm">{t('toolsLabel')}</Label>
            <div className="flex flex-wrap gap-2">
              {tools.map((tool) => {
                const on = toolIds.includes(tool.id)
                return (
                  <Toggle
                    key={tool.id}
                    pressed={on}
                    onPressedChange={(next) =>
                      setToolIds((prev) =>
                        next ? [...prev, tool.id] : prev.filter((id) => id !== tool.id),
                      )
                    }
                    variant="outline"
                    size="sm"
                    className="px-3 text-xs"
                  >
                    {tool.name}
                  </Toggle>
                )
              })}
            </div>
            <FieldDescription>{t('toolsDesc')}</FieldDescription>
          </Field>
        </FieldGroup>

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
