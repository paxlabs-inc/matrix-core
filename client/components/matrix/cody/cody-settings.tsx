'use client'

/**
 * Project settings (NEO-WORKBENCH req 8.1) — rename, archive, and delete
 * against the Neo daemon's project registry. The default project is
 * synthesized from the bare workspace root, so its controls are fixed.
 * Layers separate by background tone only — no border strokes.
 */
import { useEffect, useState } from 'react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { deleteProject, updateProject, type NeoProject } from '@/lib/api/workspace'

export function CodySettings({
  project,
  onProjectChanged,
  onProjectDeleted,
}: {
  project: NeoProject | null
  onProjectChanged: (p: NeoProject) => void
  onProjectDeleted: (id: string) => void
}) {
  const [name, setName] = useState(project?.name ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [purge, setPurge] = useState(false)

  useEffect(() => {
    setName(project?.name ?? '')
    setConfirmDelete(false)
    setError(null)
  }, [project?.id, project?.name])

  if (!project) {
    return (
      <div className="text-muted-foreground grid h-full place-items-center text-sm">
        Pick a project first.
      </div>
    )
  }
  const isDefault = project.id === 'default'

  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
    } catch (e) {
      setError(e instanceof Error ? e.message : 'That did not work — try again.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-xl flex-col gap-4 p-6">
      <section className="bg-surface-secondary flex flex-col gap-3 rounded-xl p-4">
        <div className="text-sm font-medium">Project</div>
        <div className="flex flex-col gap-2">
          <Label htmlFor="neo-settings-name">Name</Label>
          <div className="flex gap-2">
            <Input
              id="neo-settings-name"
              value={name}
              disabled={isDefault || busy}
              onChange={(e) => setName(e.target.value)}
            />
            <Button
              disabled={isDefault || busy || !name.trim() || name.trim() === project.name}
              onClick={() =>
                run(async () => {
                  const updated = await updateProject(project.id, { name: name.trim() })
                  onProjectChanged(updated)
                })
              }
            >
              Rename
            </Button>
          </div>
          {isDefault ? (
            <p className="text-muted-foreground text-xs">
              This is your main workspace — it cannot be renamed or removed.
            </p>
          ) : null}
        </div>
        <div className="text-muted-foreground font-mono text-xs">{project.root}</div>
        {!isDefault ? (
          <div>
            <Button
              variant="secondary"
              disabled={busy}
              onClick={() =>
                run(async () => {
                  const updated = await updateProject(project.id, {
                    archived: !project.archived,
                  })
                  onProjectChanged(updated)
                })
              }
            >
              {project.archived ? 'Restore' : 'Archive'}
            </Button>
          </div>
        ) : null}
      </section>

      {!isDefault ? (
        <section className="bg-surface-secondary flex flex-col gap-3 rounded-xl p-4">
          <div className="text-destructive text-sm font-medium">Danger</div>
          {!confirmDelete ? (
            <div>
              <Button variant="destructive" disabled={busy} onClick={() => setConfirmDelete(true)}>
                Delete project…
              </Button>
            </div>
          ) : (
            <div className="flex flex-col gap-2">
              <label className="flex items-center gap-2 text-xs">
                <input
                  type="checkbox"
                  checked={purge}
                  onChange={(e) => setPurge(e.target.checked)}
                />
                Also delete the files on disk (cannot be undone)
              </label>
              <div className="flex gap-2">
                <Button
                  variant="destructive"
                  disabled={busy}
                  onClick={() =>
                    run(async () => {
                      await deleteProject(project.id, { purge })
                      onProjectDeleted(project.id)
                    })
                  }
                >
                  Delete {project.name}
                </Button>
                <Button variant="ghost" disabled={busy} onClick={() => setConfirmDelete(false)}>
                  Cancel
                </Button>
              </div>
            </div>
          )}
        </section>
      ) : null}

      {error ? <p className="text-destructive text-xs">{error}</p> : null}
    </div>
  )
}
