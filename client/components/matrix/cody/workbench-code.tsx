'use client'

/**
 * The workbench Code view (NEO-WORKBENCH req 5, 6): the react-arborist file
 * tree beside an editable CodeMirror editor with an open-file tab strip.
 * Every opened file keeps its own live buffer (unsaved edits survive tab
 * switches); saves go through the workspace write API with the version hash;
 * dirty buffers show unsaved dots on the tab and header; a Neo write over an
 * unsaved buffer surfaces an explicit take-Neo's / keep-mine conflict (never
 * a silent clobber); a file Neo is actively writing renders read-only with
 * the LIVE-TYPED content streaming in.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import { CodyTree } from '@/components/matrix/cody/cody-tree'
import { CodeEditor } from '@/components/matrix/cody/code-editor'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconClose, IconCopy, IconDownload } from '@/components/matrix/cody/icons'
import {
  editBuffer,
  foldNeoWrite,
  openBuffer,
  resolveConflict,
  saveBuffer,
  type EditorBuffer,
} from '@/lib/cody/editor-buffer'
import { getFile } from '@/lib/api/workspace'
import { typingFor } from '@/lib/cody/paths'
import { cn } from '@/lib/utils'
import type { NeoTyping } from '@/hooks/api/useChat'

export function WorkbenchCode({
  project,
  selectedPath,
  onSelectPath,
  typing,
  neoWriting,
  refreshNonce = 0,
  onDirtyChange,
}: {
  project?: string
  /** The open (project-relative) file. */
  selectedPath: string | null
  onSelectPath: (path: string | null) => void
  /** Live-typing buffers from the chat reducer (keys = raw daemon paths). */
  typing?: Record<string, NeoTyping>
  /** True when Neo is actively writing the SELECTED file (editor locks). */
  neoWriting?: boolean
  /** Bumped when a Neo write settles — refetches the open file honestly. */
  refreshNonce?: number
  onDirtyChange?: (path: string, dirty: boolean) => void
}) {
  const [tabs, setTabs] = useState<string[]>([])
  const [buffers, setBuffers] = useState<Record<string, EditorBuffer>>({})
  const [loadingPath, setLoadingPath] = useState<string | null>(null)
  const buffer = selectedPath ? (buffers[selectedPath] ?? null) : null
  const bufferRef = useRef(buffer)
  bufferRef.current = buffer

  // A project switch is a different workspace — every buffer belongs to it.
  const projectRef = useRef(project)
  useEffect(() => {
    if (projectRef.current === project) return
    projectRef.current = project
    setTabs([])
    setBuffers({})
    setLoadingPath(null)
  }, [project])

  // Selecting a file opens its tab; a first open is a real read (content +
  // version hash). A re-selected tab reuses its live buffer as-is.
  useEffect(() => {
    if (!selectedPath) return
    setTabs((cur) => (cur.includes(selectedPath) ? cur : [...cur, selectedPath]))
    if (bufferRef.current) return
    let live = true
    setLoadingPath(selectedPath)
    openBuffer(project, selectedPath)
      .then((buf) => {
        if (!live) return
        setBuffers((cur) => ({ ...cur, [buf.path]: buf }))
        setLoadingPath((p) => (p === selectedPath ? null : p))
      })
      .catch(() => {
        if (!live) return
        setBuffers((cur) => ({
          ...cur,
          [selectedPath]: {
            path: selectedPath,
            content: '',
            baseHash: '',
            dirty: false,
            truncated: false,
            error: 'Could not open the file.',
          },
        }))
        setLoadingPath((p) => (p === selectedPath ? null : p))
      })
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project, selectedPath])

  // A settled Neo write to the open file: refetch and fold — a clean buffer
  // converges, a dirty buffer surfaces the conflict.
  useEffect(() => {
    if (refreshNonce === 0) return
    const buf = bufferRef.current
    if (!buf) return
    let live = true
    getFile(project, buf.path)
      .then((file) => {
        if (!live) return
        setBuffers((cur) =>
          cur[file.path] ? { ...cur, [file.path]: foldNeoWrite(cur[file.path], file) } : cur,
        )
      })
      .catch(() => {
        /* transient read failure — keep the buffer as-is */
      })
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshNonce])

  const setBufferFor = useCallback((path: string, fold: (buf: EditorBuffer) => EditorBuffer) => {
    setBuffers((cur) => (cur[path] ? { ...cur, [path]: fold(cur[path]) } : cur))
  }, [])

  const save = useCallback(() => {
    const buf = bufferRef.current
    if (!buf || buf.saving || buf.truncated || !buf.dirty) return
    setBufferFor(buf.path, (cur) => ({ ...cur, saving: true }))
    saveBuffer(project, buf)
      .then((next) => setBufferFor(next.path, () => next))
      .catch((e: Error) =>
        setBufferFor(buf.path, (cur) => ({ ...cur, saving: false, error: e.message })),
      )
  }, [project, setBufferFor])

  const onEdit = useCallback(
    (content: string) => {
      const path = bufferRef.current?.path
      if (!path) return
      setBuffers((cur) => {
        const buf = cur[path]
        if (!buf) return cur
        const next = editBuffer(buf, content)
        if (next.dirty !== buf.dirty) onDirtyChange?.(path, next.dirty)
        return { ...cur, [path]: next }
      })
    },
    [onDirtyChange],
  )

  const closeTab = useCallback(
    (path: string) => {
      const buf = buffers[path]
      if (buf?.dirty && !window.confirm(`Discard unsaved changes to ${path}?`)) return
      setTabs((cur) => {
        const next = cur.filter((p) => p !== path)
        if (path === selectedPath) {
          const at = cur.indexOf(path)
          onSelectPath(next[Math.min(at, next.length - 1)] ?? null)
        }
        return next
      })
      setBuffers((cur) => {
        const { [path]: _closed, ...rest } = cur
        return rest
      })
    },
    [buffers, selectedPath, onSelectPath],
  )

  const copyFile = useCallback(() => {
    const buf = bufferRef.current
    if (buf) void navigator.clipboard?.writeText(buf.content)
  }, [])

  const downloadFile = useCallback(() => {
    const buf = bufferRef.current
    if (!buf) return
    const url = URL.createObjectURL(new Blob([buf.content], { type: 'text/plain' }))
    const a = document.createElement('a')
    a.href = url
    a.download = buf.path.split('/').pop() || 'file.txt'
    a.click()
    URL.revokeObjectURL(url)
  }, [])

  const liveTyping = selectedPath ? typingFor(typing, selectedPath, project) : undefined
  const showTyping = !!neoWriting && !!liveTyping && !liveTyping.dropped
  const editorContent = showTyping ? liveTyping.content : (buffer?.content ?? '')
  const readOnly = !!neoWriting || !!buffer?.truncated || !!buffer?.conflict
  const loading = !!selectedPath && loadingPath === selectedPath && !buffer

  return (
    <div className="flex h-full min-h-0 gap-2 p-2">
      <div className="w-56 shrink-0">
        <CodyTree
          projectID={project}
          onOpen={onSelectPath}
          selected={selectedPath}
          className="bg-surface-secondary"
        />
      </div>

      <div className="bg-surface-secondary flex min-h-0 flex-1 flex-col rounded-lg">
        {tabs.length > 0 ? (
          <div
            role="tablist"
            aria-label="Open files"
            className="flex h-9 shrink-0 items-center gap-1 overflow-x-auto px-2 pt-1.5"
          >
            {tabs.map((path) => {
              const active = path === selectedPath
              const dirty = !!buffers[path]?.dirty
              return (
                <div
                  key={path}
                  className={cn(
                    'group flex shrink-0 items-center gap-1 rounded-md',
                    active
                      ? 'bg-surface-tertiary-alt text-foreground'
                      : 'text-muted-foreground hover:bg-surface-tertiary hover:text-foreground',
                  )}
                >
                  <button
                    role="tab"
                    aria-selected={active}
                    title={path}
                    onClick={() => onSelectPath(path)}
                    className="flex items-center gap-1.5 py-1 pl-2.5 font-mono text-xs"
                  >
                    <span className="max-w-40 truncate">{path.split('/').pop()}</span>
                    {dirty ? (
                      <span
                        aria-label="unsaved changes"
                        className="size-1.5 shrink-0 rounded-full bg-[var(--ring)]"
                      />
                    ) : null}
                  </button>
                  <button
                    aria-label={`Close ${path}`}
                    onClick={() => closeTab(path)}
                    className={cn(
                      'hover:bg-surface-hover mr-1 rounded p-0.5',
                      active ? '' : 'opacity-0 group-hover:opacity-100',
                    )}
                  >
                    <IconClose className="size-3" />
                  </button>
                </div>
              )
            })}
          </div>
        ) : null}

        {selectedPath ? (
          <>
            <div className="flex h-9 shrink-0 items-center gap-2 px-3">
              <span className="text-muted-foreground min-w-0 truncate font-mono text-xs">
                {selectedPath}
              </span>
              {buffer?.dirty ? (
                <span
                  aria-label="unsaved changes"
                  data-testid="unsaved-dot"
                  className="size-1.5 shrink-0 rounded-full bg-[var(--ring)]"
                />
              ) : null}
              {neoWriting ? (
                <span className="text-muted-foreground font-mono text-[10px] uppercase">
                  Neo is writing…
                </span>
              ) : null}
              <div className="ml-auto flex items-center gap-1">
                {buffer?.error ? (
                  <span className="text-destructive text-xs">{buffer.error}</span>
                ) : null}
                <button
                  aria-label="Copy file contents"
                  title="Copy file contents"
                  onClick={copyFile}
                  disabled={!buffer}
                  className="text-muted-foreground hover:bg-surface-hover hover:text-foreground rounded-md p-1.5 disabled:opacity-40"
                >
                  <IconCopy className="size-3.5" />
                </button>
                <button
                  aria-label="Download file"
                  title="Download file"
                  onClick={downloadFile}
                  disabled={!buffer}
                  className="text-muted-foreground hover:bg-surface-hover hover:text-foreground rounded-md p-1.5 disabled:opacity-40"
                >
                  <IconDownload className="size-3.5" />
                </button>
                <button
                  onClick={save}
                  disabled={!buffer?.dirty || buffer.saving || readOnly}
                  className="bg-surface-submit hover:bg-surface-submit-hover ml-1 rounded-md px-2.5 py-1 text-xs disabled:opacity-40"
                >
                  {buffer?.saving ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>

            {buffer?.conflict ? (
              <ConflictBar
                mine={buffer.content}
                theirs={buffer.conflict.theirs}
                onChoose={(choice) => {
                  setBufferFor(buffer.path, (cur) => resolveConflict(cur, choice))
                  if (choice === 'keep-mine') {
                    // Re-save the user's bytes against the current version.
                    setTimeout(save, 0)
                  }
                }}
              />
            ) : null}

            <div className="min-h-0 flex-1">
              {loading ? (
                <CodyLoader variant="ring" label="Opening…" className="h-full justify-center" />
              ) : (
                <CodeEditor
                  path={selectedPath}
                  content={editorContent}
                  readOnly={readOnly}
                  onChange={onEdit}
                  onSave={save}
                />
              )}
            </div>
          </>
        ) : (
          <div className="text-muted-foreground grid h-full place-items-center text-sm">
            Pick a file to read or edit it.
          </div>
        )}
      </div>
    </div>
  )
}

/** The explicit conflict choice (req 5.3): Neo changed the file while the
 *  buffer had unsaved edits. Shows both sides; the user picks. */
function ConflictBar({
  mine,
  theirs,
  onChoose,
}: {
  mine: string
  theirs: string
  onChoose: (choice: 'take-neo' | 'keep-mine') => void
}) {
  const preview = (s: string) => (s.length > 400 ? s.slice(0, 400) + '…' : s)
  const changed = useMemo(() => mine !== theirs, [mine, theirs])
  return (
    <div data-testid="conflict-bar" className="bg-surface-tertiary mx-2 mb-2 rounded-lg p-3">
      <div className="text-sm font-medium">Neo changed this file while you were editing it.</div>
      <div className="text-muted-foreground mt-1 text-xs">
        {changed
          ? 'Pick which version to keep — nothing is discarded until you choose.'
          : 'The versions are identical; either choice keeps your text.'}
      </div>
      <div className="mt-2 grid grid-cols-2 gap-2">
        <div className="bg-surface-primary-alt min-w-0 rounded-md p-2">
          <div className="text-muted-foreground mb-1 font-mono text-[10px] uppercase">
            Neo&apos;s version
          </div>
          <pre className="max-h-32 overflow-auto font-mono text-[11px] whitespace-pre-wrap">
            {preview(theirs)}
          </pre>
        </div>
        <div className="bg-surface-primary-alt min-w-0 rounded-md p-2">
          <div className="text-muted-foreground mb-1 font-mono text-[10px] uppercase">Yours</div>
          <pre className="max-h-32 overflow-auto font-mono text-[11px] whitespace-pre-wrap">
            {preview(mine)}
          </pre>
        </div>
      </div>
      <div className="mt-2 flex gap-2">
        <button
          onClick={() => onChoose('take-neo')}
          className="bg-surface-submit hover:bg-surface-submit-hover rounded-md px-2.5 py-1 text-xs"
        >
          Take Neo&apos;s
        </button>
        <button
          onClick={() => onChoose('keep-mine')}
          className="bg-surface-submit hover:bg-surface-submit-hover rounded-md px-2.5 py-1 text-xs"
        >
          Keep mine
        </button>
      </div>
    </div>
  )
}
