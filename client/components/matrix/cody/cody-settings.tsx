'use client'

/**
 * Cody Settings — project-level mode (changeable between runs) and preview TTL.
 * Mode changes hit the codyd registry (PATCH /projects/:id); the default
 * project's mode is fixed. Preview TTL is applied when a preview is provisioned
 * (backend task 3.3); surfaced here so the knob lives with the project.
 */
import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { MODE_LABELS, MODE_ORDER } from '@/components/matrix/cody/tiering'
import type { CodyMode, CodyProject } from '@/lib/api/cody'

const TTL_OPTIONS = [
  { value: '15', label: '15 minutes' },
  { value: '30', label: '30 minutes' },
  { value: '60', label: '1 hour' },
  { value: '120', label: '2 hours' },
]

export function CodySettings({
  project,
  onChangeMode,
  busy = false,
  error,
}: {
  project: CodyProject | null
  onChangeMode: (mode: CodyMode) => void
  busy?: boolean
  error?: string | null
}) {
  const isDefault = !project || project.id === 'default'
  const [ttl, setTtl] = useState('30')

  if (!project) {
    return (
      <div className="text-muted-foreground m-auto p-8 text-sm">
        Select a project to edit its settings.
      </div>
    )
  }

  return (
    <div className="mx-auto flex w-full max-w-xl flex-col gap-6 p-6">
      <div className="flex flex-col gap-2">
        <h2 className="text-sm font-medium">Mode</h2>
        <p className="text-muted-foreground text-xs">
          The project&apos;s mode sets how much of the machinery the surface reveals. Change it
          between runs, never mid-run.
        </p>
        <div className="bg-surface-secondary flex items-center gap-3 rounded-lg p-4">
          <Label htmlFor="cody-settings-mode" className="shrink-0">
            Project mode
          </Label>
          <div className="ml-auto flex items-center gap-2">
            {busy ? <CodyLoader variant="dots" /> : null}
            <Select
              value={project.mode}
              onValueChange={(v) => onChangeMode(v as CodyMode)}
              disabled={isDefault || busy}
            >
              <SelectTrigger id="cody-settings-mode" className="w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {MODE_ORDER.map((m) => (
                  <SelectItem key={m} value={m}>
                    {MODE_LABELS[m]}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>
        {isDefault ? (
          <p className="text-muted-foreground text-xs">
            The default project&apos;s mode is fixed. Create a project to choose a mode.
          </p>
        ) : null}
        {error ? <p className="text-destructive text-xs">{error}</p> : null}
      </div>

      <div className="flex flex-col gap-2">
        <h2 className="text-sm font-medium">Preview</h2>
        <div className="bg-surface-secondary flex items-center gap-3 rounded-lg p-4">
          <Label htmlFor="cody-settings-ttl" className="shrink-0">
            Idle preview TTL
          </Label>
          <Select value={ttl} onValueChange={setTtl}>
            <SelectTrigger id="cody-settings-ttl" className="ml-auto w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {TTL_OPTIONS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {o.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <p className="text-muted-foreground text-xs">
          Idle preview sandboxes are reaped after this window. Applied when a preview is
          provisioned.
        </p>
      </div>

      <div className="flex flex-col gap-1">
        <h2 className="text-sm font-medium">Workspace</h2>
        <div className="bg-surface-secondary flex flex-col gap-1 rounded-lg p-4">
          <Row label="Project" value={project.name} />
          <Row label="Root" value={project.root} mono />
          <Row label="Created" value={new Date(project.created_at).toLocaleString()} />
        </div>
      </div>

      <div className="flex justify-end">
        <Button variant="ghost" disabled>
          Saved
        </Button>
      </div>
    </div>
  )
}

function Row({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-center gap-3 py-1">
      <span className="text-muted-foreground w-20 shrink-0 text-xs">{label}</span>
      <span className={mono ? 'truncate font-mono text-xs' : 'truncate text-sm'}>{value}</span>
    </div>
  )
}
