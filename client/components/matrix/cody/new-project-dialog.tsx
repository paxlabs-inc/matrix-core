'use client'

/**
 * New-project dialog (task 5.2) — name + mode set at creation, wired to the
 * codyd project registry. Mode is a project-level setting; it is changeable
 * later in Settings, never per message.
 */
import { useState } from 'react'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
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
import type { CodyMode } from '@/lib/api/cody'

const MODE_HINT: Record<CodyMode, string> = {
  prototype: 'Vibe-code fast — preview-first, one-button undo, no machinery.',
  engineer: 'The default — waved task board, verification evidence, decision cards, checkpoints.',
  architect: 'Everything, plus the terminal, live spec viewer, and git surface.',
}

export function NewProjectDialog({
  open,
  onOpenChange,
  onCreate,
  busy = false,
  error,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreate: (input: { name: string; mode: CodyMode }) => void
  busy?: boolean
  error?: string | null
}) {
  const [name, setName] = useState('')
  const [mode, setMode] = useState<CodyMode>('engineer')

  const submit = () => {
    const trimmed = name.trim()
    if (!trimmed) return
    onCreate({ name: trimmed, mode })
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>
            Each project has its own workspace and its own mode. Set the mode now — you can change
            it between runs in Settings.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-2">
          <div className="flex flex-col gap-2">
            <Label htmlFor="cody-project-name">Name</Label>
            <Input
              id="cody-project-name"
              value={name}
              autoFocus
              placeholder="my-app"
              onChange={(e) => setName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') submit()
              }}
            />
          </div>

          <div className="flex flex-col gap-2">
            <Label htmlFor="cody-project-mode">Mode</Label>
            <Select value={mode} onValueChange={(v) => setMode(v as CodyMode)}>
              <SelectTrigger id="cody-project-mode">
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
            <p className="text-muted-foreground text-xs">{MODE_HINT[mode]}</p>
          </div>

          {error ? <p className="text-destructive text-xs">{error}</p> : null}
        </div>

        <DialogFooter>
          <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={busy || !name.trim()}>
            {busy ? <CodyLoader variant="dots" /> : 'Create project'}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
