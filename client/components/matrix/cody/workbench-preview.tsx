'use client'

/**
 * The workbench Preview pane (NEO-WORKBENCH req 7.2, 7.3): the sandbox URL in
 * an iframe with bolt.diy's chrome — an editable address-bar PATH, reload,
 * and open-in-new-tab. States are honest: pending/failed/expired render as
 * real empty/error states; a stale frame is never presented as live.
 */
import { useEffect, useMemo, useState } from 'react'

import type { NeoPreview } from '@/hooks/api/useChat'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconExternal, IconRefresh } from '@/components/matrix/cody/icons'

export function WorkbenchPreview({
  preview,
  onRequestPreview,
}: {
  preview?: NeoPreview
  /** Ask the daemon for a (fresh) sandbox preview of the project. */
  onRequestPreview?: () => void
}) {
  const [path, setPath] = useState('/')
  const [nonce, setNonce] = useState(0)

  const base = preview?.state === 'ready' ? (preview.url ?? '') : ''
  // A NEW ready URL resets the path bar to the root.
  useEffect(() => {
    setPath('/')
  }, [base])

  const src = useMemo(() => {
    if (!base) return ''
    try {
      const u = new URL(path || '/', base)
      return u.toString()
    } catch {
      return base
    }
  }, [base, path])

  if (!preview) {
    return (
      <EmptyState
        title="No preview yet"
        detail="Start one to run the app in a fresh sandbox — never on your main machine."
        action={
          onRequestPreview ? { label: 'Start preview', onClick: onRequestPreview } : undefined
        }
      />
    )
  }
  if (preview.state === 'pending') {
    return (
      <div className="grid h-full place-items-center">
        <CodyLoader variant="ring" label="Starting the preview sandbox…" />
      </div>
    )
  }
  if (preview.state === 'failed') {
    return (
      <EmptyState
        title="The preview could not start"
        detail={preview.reason || 'The dev server did not come up.'}
        action={onRequestPreview ? { label: 'Try again', onClick: onRequestPreview } : undefined}
      />
    )
  }
  if (preview.state === 'expired') {
    return (
      <EmptyState
        title="The preview expired"
        detail="Idle sandboxes are torn down to keep things tidy."
        action={
          onRequestPreview ? { label: 'Start it again', onClick: onRequestPreview } : undefined
        }
      />
    )
  }

  return (
    <div className="flex h-full min-h-0 flex-col gap-2 p-2">
      <div className="bg-surface-secondary flex h-9 shrink-0 items-center gap-2 rounded-lg px-2">
        <button
          aria-label="Reload preview"
          onClick={() => setNonce((n) => n + 1)}
          className="text-muted-foreground hover:bg-surface-hover hover:text-foreground rounded-md p-1.5"
        >
          <IconRefresh className="size-3.5" />
        </button>
        <span className="text-muted-foreground max-w-[40%] truncate font-mono text-[11px]">
          {base.replace(/^https?:\/\//, '')}
        </span>
        <input
          aria-label="Preview path"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') setNonce((n) => n + 1)
          }}
          className="bg-surface-primary-alt min-w-0 flex-1 rounded-md px-2 py-1 font-mono text-xs outline-none"
          placeholder="/"
        />
        <a
          href={src}
          target="_blank"
          rel="noreferrer"
          aria-label="Open preview in a new tab"
          className="text-muted-foreground hover:bg-surface-hover hover:text-foreground rounded-md p-1.5"
        >
          <IconExternal className="size-3.5" />
        </a>
      </div>
      <iframe
        key={`${src}-${nonce}`}
        src={src}
        title="App preview"
        data-testid="preview-frame"
        className="min-h-0 w-full flex-1 rounded-lg bg-white"
        sandbox="allow-scripts allow-forms allow-same-origin allow-popups allow-modals"
      />
    </div>
  )
}

function EmptyState({
  title,
  detail,
  action,
}: {
  title: string
  detail: string
  action?: { label: string; onClick: () => void }
}) {
  return (
    <div data-testid="preview-empty" className="grid h-full place-items-center p-6">
      <div className="max-w-sm text-center">
        <div className="text-sm font-medium">{title}</div>
        <div className="text-muted-foreground mt-1 text-xs">{detail}</div>
        {action ? (
          <button
            onClick={action.onClick}
            className="bg-surface-submit hover:bg-surface-submit-hover mt-3 rounded-md px-3 py-1.5 text-xs"
          >
            {action.label}
          </button>
        ) : null}
      </div>
    </div>
  )
}
