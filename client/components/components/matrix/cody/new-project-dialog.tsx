'use client'

/**
 * New-project dialog (NEO-WORKBENCH req 8.1) — a name is all it takes; the
 * daemon derives the workspace subdirectory. No mode tiers: one workbench
 * serves everyone.
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
import { CodyLoader } from '@/components/matrix/cody/loaders'

export function NewProjectDialog({
  open,
  onOpenChange,
  onCreate,
  busy = false,
  error,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreate: (input: { name: string }) => void
  busy?: boolean
  error?: string | null
}) {
  const [name, setName] = useState('')

  const submit = () => {
    const trimmed = name.trim()
    if (!trimmed) return
    onCreate({ name: trimmed })
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>
            Each project is its own folder in your workspace, with its own history.
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-4 py-2">
          <div className="flex flex-col gap-2">
            <Label htmlFor="neo-project-name">Name</Label>
            <Input
              id="neo-project-name"
              value={name}
              autoFocus
              placeholder="my-app"
              onChange={(e) => setName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') submit()
              }}
            />
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
