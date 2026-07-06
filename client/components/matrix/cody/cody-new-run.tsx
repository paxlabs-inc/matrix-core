'use client'

/**
 * Cody new-run entry (task 6.7) — spec adoption on top of the plain composer.
 *
 * Extends the workspace Composer with three ways to start a run:
 *   - Prose: state the goal plainly (the existing default).
 *   - Paste spec: drop a spec document; the run adopts it (submits `spec`).
 *   - Adopt file: browse the workspace tree and pick a spec file to adopt
 *     (submits `specPath`).
 *
 * `onStart` hands the chosen shape up; the caller maps it onto submitChat.
 * Copy follows the mode register (`outcome`), matching the Composer idiom.
 * Mode selection and layers separate by background tone — no border strokes.
 */
import { useEffect, useState } from 'react'

import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconPlay, IconFileText, IconAlertCircle } from '@/components/matrix/cody/icons'
import { getTree, type TreeEntry } from '@/lib/api/cody'
import { cn } from '@/lib/utils'

type Mode = 'prose' | 'paste' | 'file'

export interface NewRunInput {
  message?: string
  spec?: string
  specPath?: string
}

export function CodyNewRun({
  projectID,
  outcome,
  onStart,
}: {
  projectID?: string
  outcome: boolean
  onStart: (input: NewRunInput) => void
}) {
  const [mode, setMode] = useState<Mode>('prose')
  const [prose, setProse] = useState('')
  const [spec, setSpec] = useState('')
  const [specPath, setSpecPath] = useState('')

  const submit = () => {
    if (mode === 'prose') {
      const t = prose.trim()
      if (!t) return
      onStart({ message: t })
    } else if (mode === 'paste') {
      const t = spec.trim()
      if (!t) return
      onStart({ spec: t })
    } else {
      if (!specPath) return
      onStart({ specPath })
    }
  }

  const canStart = mode === 'prose' ? !!prose.trim() : mode === 'paste' ? !!spec.trim() : !!specPath

  return (
    <div className="m-auto flex w-full max-w-2xl flex-col gap-4 p-6">
      <div className="flex flex-col gap-1">
        <h1 className="text-xl font-medium">
          {outcome ? 'What do you want to build?' : 'Describe the build'}
        </h1>
        <p className="text-muted-foreground text-sm">
          {outcome
            ? 'Say it plainly, paste a spec, or adopt one from your workspace. Cody plans it, builds it, and shows you a working preview.'
            : 'State the goal, paste a spec document, or adopt a spec file. Cody authors a plan, decides the stack, and executes it wave by wave.'}
        </p>
      </div>

      <ModeSwitch mode={mode} onChange={setMode} />

      {mode === 'prose' ? (
        <Textarea
          value={prose}
          autoFocus
          rows={4}
          placeholder={
            outcome ? 'A recipe app where I can save favorites…' : 'A realtime chat service with…'
          }
          onChange={(e) => setProse(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit()
          }}
        />
      ) : null}

      {mode === 'paste' ? (
        <div className="flex flex-col gap-2">
          <p className="text-muted-foreground text-xs">
            Paste a spec document. Cody adopts it as the plan and records its provenance.
          </p>
          <Textarea
            value={spec}
            autoFocus
            rows={10}
            className="font-mono text-xs"
            placeholder={'# Goal\n\n## Requirements\n- …\n\n## Tasks\n- …'}
            onChange={(e) => setSpec(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit()
            }}
          />
        </div>
      ) : null}

      {mode === 'file' ? (
        <FilePicker projectID={projectID} selected={specPath} onSelect={setSpecPath} />
      ) : null}

      <div className="flex items-center justify-between">
        <span className="text-muted-foreground font-mono text-[11px]">
          {mode === 'file'
            ? specPath
              ? `adopting ${specPath}`
              : 'pick a file to adopt'
            : '⌘↵ to start'}
        </span>
        <Button onClick={submit} disabled={!canStart}>
          <IconPlay className="size-3.5" />
          <span>{mode === 'prose' ? 'Start' : 'Adopt & start'}</span>
        </Button>
      </div>
    </div>
  )
}

// ── Mode switch (background-tone segmented control) ───────────────────────────

function ModeSwitch({ mode, onChange }: { mode: Mode; onChange: (m: Mode) => void }) {
  const options: { id: Mode; label: string }[] = [
    { id: 'prose', label: 'Prose goal' },
    { id: 'paste', label: 'Paste spec' },
    { id: 'file', label: 'Adopt file' },
  ]
  return (
    <div className="bg-surface-secondary flex w-fit gap-1 rounded-lg p-1">
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          onClick={() => onChange(o.id)}
          className={cn(
            'rounded-md px-3 py-1.5 text-sm font-medium transition-colors',
            mode === o.id
              ? 'bg-surface-hover text-foreground'
              : 'text-muted-foreground hover:bg-surface-tertiary',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

// ── Workspace file picker ─────────────────────────────────────────────────────

function FilePicker({
  projectID,
  selected,
  onSelect,
}: {
  projectID?: string
  selected: string
  onSelect: (path: string) => void
}) {
  const [entries, setEntries] = useState<TreeEntry[] | null>(null)
  const [truncated, setTruncated] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const ctrl = new AbortController()
    setEntries(null)
    setError(null)
    getTree(projectID, ctrl.signal)
      .then((tree) => {
        setEntries(tree.entries.filter((e) => !e.dir))
        setTruncated(tree.truncated)
      })
      .catch((err: unknown) => {
        if (ctrl.signal.aborted) return
        setError(err instanceof Error ? err.message : 'Could not load the workspace tree.')
      })
    return () => ctrl.abort()
  }, [projectID])

  if (error) {
    return (
      <div className="bg-surface-secondary flex items-center gap-2 rounded-lg p-4 text-sm">
        <IconAlertCircle className="text-destructive size-4 shrink-0" />
        <span className="text-muted-foreground">{error}</span>
      </div>
    )
  }

  if (!entries) {
    return (
      <div className="bg-surface-secondary rounded-lg p-6">
        <CodyLoader variant="ring" label="Loading workspace…" className="m-auto" />
      </div>
    )
  }

  if (entries.length === 0) {
    return (
      <div className="bg-surface-secondary rounded-lg p-4 text-sm">
        <span className="text-muted-foreground">
          No files in this workspace yet. Paste a spec or state the goal instead.
        </span>
      </div>
    )
  }

  return (
    <div className="bg-surface-secondary flex max-h-72 flex-col gap-0.5 overflow-y-auto rounded-lg p-2">
      {entries.map((e) => (
        <button
          key={e.path}
          type="button"
          onClick={() => onSelect(e.path)}
          className={cn(
            'flex items-center gap-2 rounded-md px-3 py-2 text-left text-sm transition-colors',
            selected === e.path
              ? 'bg-surface-hover text-foreground'
              : 'text-muted-foreground hover:bg-surface-tertiary',
          )}
        >
          <IconFileText className="size-4 shrink-0" />
          <span className="truncate font-mono text-xs">{e.path}</span>
        </button>
      ))}
      {truncated ? (
        <p className="text-muted-foreground px-3 py-2 font-mono text-[10px]">
          tree truncated — some files are not shown
        </p>
      ) : null}
    </div>
  )
}
