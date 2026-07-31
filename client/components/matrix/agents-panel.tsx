'use client'

import { Bot } from '@/lib/matrix-icons'
import { useTranslations } from 'next-intl'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/matrix/astryx-card'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/matrix/astryx-switch'
import { Label } from '@/components/ui/label'
import { agents, tools, type Agent, type Tool } from '@/lib/matrix-data'

export function AgentsPanel({ items = agents }: { items?: Agent[] }) {
  const t = useTranslations('agentsPanel')

  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
      {items.map((a) => (
        <Card key={a.id}>
          <CardHeader>
            <div className="flex items-start justify-between gap-3">
              <div className="flex items-center gap-3">
                <span className="bg-primary/10 text-primary flex size-9 items-center justify-center rounded-lg">
                  <Bot className="size-5" />
                </span>
                <div className="flex flex-col">
                  <CardTitle className="text-base">{a.name}</CardTitle>
                  <CardDescription>{a.role}</CardDescription>
                </div>
              </div>
              <Badge variant={a.status === 'active' ? 'default' : 'secondary'}>
                {a.status === 'active' ? t('active') : t('idle')}
              </Badge>
            </div>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <p className="text-muted-foreground text-sm text-pretty">{a.description}</p>
            <div className="border-border flex items-center gap-6 border-t pt-3">
              <Stat label={t('runsCompleted')} value={a.runsCompleted.toLocaleString()} />
              <Stat label={t('successRate')} value={`${Math.round(a.successRate * 100)}%`} />
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

export function ToolsPanel({ items = tools }: { items?: Tool[] }) {
  const t = useTranslations('agentsPanel')

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{t('toolsTitle')}</CardTitle>
        <CardDescription>{t('toolsDesc')}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-1 px-0">
        {items.map((tool) => (
          <div
            key={tool.id}
            className="hover:bg-muted/40 flex items-center justify-between gap-4 px-6 py-3 transition-colors"
          >
            <div className="flex flex-col gap-0.5">
              <div className="flex items-center gap-2">
                <Label htmlFor={tool.id} className="text-sm font-medium">
                  {tool.name}
                </Label>
                <span className="border-border bg-muted text-muted-foreground rounded border px-1.5 py-0.5 font-mono text-[10px]">
                  {tool.category}
                </span>
              </div>
              <span className="text-muted-foreground text-xs">{tool.description}</span>
            </div>
            <Switch id={tool.id} defaultChecked={tool.enabled} />
          </div>
        ))}
      </CardContent>
    </Card>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col">
      <span className="text-foreground font-mono text-lg font-semibold tabular-nums">{value}</span>
      <span className="text-muted-foreground text-xs">{label}</span>
    </div>
  )
}
