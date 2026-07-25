'use client'

/**
 * The ONE coding workspace (NEO-WORKBENCH req 1, 2, 6): a real Neo chat rail
 * as the primary column, with the workbench sliding in from the right when
 * work arrives (or via the manual toggle). The tier system is retired — this
 * single layout serves every user.
 *
 * Owns the workbench UI state (open/view/terminal/selected file) and the
 * bolt-style force-follow: when Neo starts writing a file, the workbench
 * auto-opens on Code view with that file selected — unless the user pinned a
 * view themselves this run.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import type { UseChatResult } from '@/hooks/api/useChat'
import { ChatRail } from '@/components/matrix/cody/chat-rail'
import { NeoWorkbench, type WorkbenchView } from '@/components/matrix/cody/workbench'
import { WorkbenchCode } from '@/components/matrix/cody/workbench-code'
import { WorkbenchPreview } from '@/components/matrix/cody/workbench-preview'
import { CodyDiffView } from '@/components/matrix/cody/cody-diff'
import { CodyShell } from '@/components/matrix/cody/cody-xterm'
import { requestPreview } from '@/lib/api/workspace'
import { relWorkspacePath } from '@/lib/cody/paths'

export function CodyWorkspace({
  chat,
  project,
  projectName,
}: {
  chat: UseChatResult
  /** The active project id ('default' → the bare workspace root). */
  project?: string
  projectName?: string
}) {
  const { task } = chat
  const [open, setOpen] = useState(false)
  const [view, setView] = useState<WorkbenchView>('code')
  const [terminalOpen, setTerminalOpen] = useState(false)
  // xterm boots on first reveal and STAYS mounted after (scrollback survives
  // the toggle) without paying its cost for users who never open it.
  const [terminalBooted, setTerminalBooted] = useState(false)
  const [selectedPath, setSelectedPath] = useState<string | null>(null)
  const [refreshNonce, setRefreshNonce] = useState(0)

  // View pinning: a manual view change during a run wins over force-follow.
  const pinnedRef = useRef(false)
  const runRef = useRef<string | null>(null)
  useEffect(() => {
    const intentId = task?.intentId ?? null
    if (intentId !== runRef.current) {
      runRef.current = intentId
      pinnedRef.current = false
    }
  }, [task?.intentId])

  const onViewChange = useCallback((v: WorkbenchView) => {
    pinnedRef.current = true
    setView(v)
  }, [])

  // The paths Neo is writing RIGHT NOW (running editor write steps).
  const writingPaths = useMemo(() => {
    const out: string[] = []
    for (const s of task?.steps ?? []) {
      if (
        s.kind === 'editor' &&
        s.running &&
        s.path &&
        (s.action === 'write' || s.action === 'edit')
      ) {
        out.push(s.path)
      }
    }
    return out
  }, [task?.steps])

  // Slide in when work arrives (req 2.2): the first workbench-relevant step
  // opens the panel.
  const hasWork = useMemo(
    () =>
      (task?.steps ?? []).some(
        (s) => s.kind === 'editor' || s.kind === 'terminal' || s.kind === 'browser',
      ),
    [task?.steps],
  )
  const sawWorkRef = useRef(false)
  useEffect(() => {
    if (hasWork && !sawWorkRef.current) {
      sawWorkRef.current = true
      setOpen(true)
    }
    if (!hasWork) sawWorkRef.current = false
  }, [hasWork])

  // Force-follow (req 6.2): a starting file write auto-opens Code view on
  // that file, unless the user pinned a different view this run.
  const followedRef = useRef<string | null>(null)
  useEffect(() => {
    const raw = writingPaths[writingPaths.length - 1]
    if (!raw || followedRef.current === raw) return
    followedRef.current = raw
    setOpen(true)
    if (!pinnedRef.current) setView('code')
    setSelectedPath(relWorkspacePath(raw, project))
  }, [writingPaths, project])

  // A settled write to the selected file refreshes the buffer (convergence
  // to the authoritative disk bytes; conflict if the user had unsaved edits).
  const settledWrites = useMemo(() => {
    let n = 0
    for (const s of task?.steps ?? []) {
      if (s.kind === 'editor' && !s.running && (s.action === 'write' || s.action === 'edit')) n++
    }
    return n
  }, [task?.steps])
  const settledRef = useRef(0)
  useEffect(() => {
    if (settledWrites > settledRef.current) setRefreshNonce((x) => x + 1)
    settledRef.current = settledWrites
  }, [settledWrites])

  const neoWritingSelected = useMemo(() => {
    if (!selectedPath) return false
    return writingPaths.some(
      (raw) => relWorkspacePath(raw, project) === selectedPath || raw.endsWith('/' + selectedPath),
    )
  }, [writingPaths, selectedPath, project])

  const openFileFromCard = useCallback(
    (rawPath: string) => {
      setSelectedPath(relWorkspacePath(rawPath, project))
      setView('code')
      setOpen(true)
    },
    [project],
  )

  const revealTerminal = useCallback(() => {
    setOpen(true)
    setTerminalOpen(true)
    setTerminalBooted(true)
  }, [])

  const onRequestPreview = useCallback(() => {
    if (!chat.conversationId) return
    void requestPreview(project, chat.conversationId).catch(() => {
      /* the failure surfaces as a durable preview.failed event */
    })
  }, [chat.conversationId, project])

  const stop = useCallback(() => {
    chat.dismissTask()
  }, [chat])

  return (
    <div className="relative flex h-full min-h-0 flex-1 overflow-hidden">
      <div className="flex h-full min-h-0 min-w-0 flex-1 flex-col">
        <ChatRail
          chat={chat}
          projectName={projectName}
          onOpenFile={openFileFromCard}
          onRevealTerminal={revealTerminal}
          onStop={stop}
        />
      </div>

      {/* Manual toggle lives on the rail edge so the workbench is reachable
          before any work arrives. */}
      {!open ? (
        <button
          aria-label="Open workbench"
          onClick={() => setOpen(true)}
          className="text-muted-foreground hover:text-foreground bg-surface-secondary my-2 grid min-h-11 min-w-11 shrink-0 place-items-center rounded-l-lg font-mono text-sm"
        >
          ‹
        </button>
      ) : null}

      <NeoWorkbench
        open={open}
        view={view}
        onViewChange={onViewChange}
        onClose={() => setOpen(false)}
        terminalOpen={terminalOpen}
        onToggleTerminal={() => {
          setTerminalOpen((t) => !t)
          setTerminalBooted(true)
        }}
        previewReady={task?.preview?.state === 'ready'}
        code={
          <WorkbenchCode
            project={project}
            selectedPath={selectedPath}
            onSelectPath={setSelectedPath}
            typing={task?.typing}
            neoWriting={neoWritingSelected}
            refreshNonce={refreshNonce}
          />
        }
        diff={
          <div className="h-full min-h-0 p-2">
            <CodyDiffView projectID={project} />
          </div>
        }
        preview={<WorkbenchPreview preview={task?.preview} onRequestPreview={onRequestPreview} />}
        terminal={
          terminalBooted ? (
            <div className="h-full min-h-0 px-2 pb-2">
              <CodyShell projectID={project} />
            </div>
          ) : null
        }
      />
    </div>
  )
}
