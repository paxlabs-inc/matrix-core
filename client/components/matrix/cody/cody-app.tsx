'use client'

/**
 * CodyApp — the /cody sibling app (task 5.1). Owns the shell (sidebar + top bar
 * + tier-driven viewport), the project registry, and the current run. The Neo
 * dashboard is a separate route and is untouched.
 *
 * Transport is reuse-only: runs fold through useCody (shared SSE hub + trace
 * rebuild); projects and actions go through lib/api/cody over the shared
 * apiFetch. Disclosure follows the active project's mode tier.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'

import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { CodySidebar, type CodyPage } from '@/components/matrix/cody/cody-sidebar'
import { CodyTopbar } from '@/components/matrix/cody/cody-topbar'
import { CodyWorkspace } from '@/components/matrix/cody/cody-workspace'
import { CodyHistory } from '@/components/matrix/cody/cody-history'
import { CodySettings } from '@/components/matrix/cody/cody-settings'
import { NewProjectDialog } from '@/components/matrix/cody/new-project-dialog'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { tierFor } from '@/components/matrix/cody/tiering'
import { useCody } from '@/hooks/api/useCody'
import type { NewRunInput } from '@/components/matrix/cody/cody-new-run'
import {
  answerIntent,
  createProject,
  listProjects,
  rePreview,
  setProjectMode,
  steerIntent,
  stopRun,
  submitChat,
  type AnswerVerdict,
  type CodyMode,
  type CodyProject,
} from '@/lib/api/cody'
import { listRecentRuns, rememberRun, type RecentRun } from '@/lib/cody/recent-runs'

export function CodyApp() {
  const [projects, setProjects] = useState<CodyProject[]>([])
  const [projectsLoaded, setProjectsLoaded] = useState(false)
  const [activeProjectId, setActiveProjectId] = useState<string | null>(null)
  const [page, setPage] = useState<CodyPage>('workspace')

  const [convByProject, setConvByProject] = useState<Record<string, string | undefined>>({})
  const [pendingAttach, setPendingAttach] = useState<{
    convID: string
    intentId: string
    mode?: CodyMode
  } | null>(null)

  const [newProjectOpen, setNewProjectOpen] = useState(false)
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)

  const [modeBusy, setModeBusy] = useState(false)
  const [modeError, setModeError] = useState<string | null>(null)

  const [recentRuns, setRecentRuns] = useState<RecentRun[]>([])

  const activeProject = useMemo(
    () => projects.find((p) => p.id === activeProjectId) ?? null,
    [projects, activeProjectId],
  )
  const tier = tierFor(activeProject?.mode)
  const convID = activeProjectId ? (convByProject[activeProjectId] ?? null) : null

  const { run, connection, attach, reload } = useCody(convID)
  const isLive =
    connection === 'open' || connection === 'connecting' || connection === 'reconnecting'

  // Load the project registry once.
  useEffect(() => {
    let cancelled = false
    listProjects()
      .then((list) => {
        if (cancelled) return
        setProjects(list)
        setActiveProjectId((cur) => cur ?? list[0]?.id ?? null)
      })
      .catch(() => {
        /* registry unreachable — surface an empty state, not a crash */
      })
      .finally(() => {
        if (!cancelled) setProjectsLoaded(true)
      })
    return () => {
      cancelled = true
    }
  }, [])

  // Refresh recent runs when the active project changes.
  useEffect(() => {
    setRecentRuns(listRecentRuns(activeProjectId ?? undefined))
  }, [activeProjectId])

  // Attach the live stream once convID has updated to the freshly dispatched run.
  useEffect(() => {
    if (pendingAttach && convID === pendingAttach.convID) {
      attach(pendingAttach.intentId, pendingAttach.mode)
      setPendingAttach(null)
    }
  }, [pendingAttach, convID, attach])

  const setConvForActive = useCallback(
    (cid: string | null) => {
      if (!activeProjectId) return
      setConvByProject((m) => ({ ...m, [activeProjectId]: cid ?? undefined }))
    },
    [activeProjectId],
  )

  const onStartRun = useCallback(
    async (input: NewRunInput) => {
      if (!activeProject) return
      // A prose goal, a pasted spec, or an adopted workspace file — codyd takes
      // message + optional spec/spec_path. Title the run by whatever was given.
      const message =
        input.message ??
        (input.spec ? 'Build from the pasted spec' : `Adopt ${input.specPath ?? 'spec'}`)
      try {
        const dispatch = await submitChat({
          message,
          spec: input.spec,
          spec_path: input.specPath,
          project_id: activeProject.id === 'default' ? undefined : activeProject.id,
          mode: activeProject.mode,
        })
        setConvForActive(dispatch.conversation_id)
        setPendingAttach({
          convID: dispatch.conversation_id,
          intentId: dispatch.intent_id,
          mode: activeProject.mode,
        })
        const remembered = rememberRun({
          convID: dispatch.conversation_id,
          projectID: activeProject.id,
          title: message.slice(0, 80),
          startedAt: Date.now(),
        })
        setRecentRuns(remembered.filter((r) => r.projectID === activeProject.id))
      } catch {
        /* dispatch failed — the entry form stays; the user can retry */
      }
    },
    [activeProject, setConvForActive],
  )

  const onRePreview = useCallback(() => {
    if (!activeProject) return
    const cid = convByProject[activeProject.id]
    if (!cid) return
    void rePreview({
      conversation_id: cid,
      projectID: activeProject.id === 'default' ? undefined : activeProject.id,
    })
  }, [activeProject, convByProject])

  const onSteer = useCallback(
    (text: string) => {
      if (run?.runId) void steerIntent(run.runId, text)
    },
    [run?.runId],
  )

  // Continue (req 3.2): re-dispatch the SAME conversation — codyd resumes the
  // durable plan under the same run id, so the transcript and attach point
  // carry on instead of starting over.
  const onContinue = useCallback(
    async (text: string) => {
      if (!activeProject || !convID) return
      try {
        const dispatch = await submitChat({
          message: text,
          conversation_id: convID,
          project_id: activeProject.id === 'default' ? undefined : activeProject.id,
          mode: activeProject.mode,
        })
        setPendingAttach({
          convID: dispatch.conversation_id,
          intentId: dispatch.intent_id,
          mode: activeProject.mode,
        })
      } catch {
        /* dispatch failed — the composer stays; the user can retry */
      }
    },
    [activeProject, convID],
  )

  const onAnswer = useCallback(
    (answer: { text?: string; verdict?: AnswerVerdict }) => {
      if (run?.runId) void answerIntent(run.runId, answer)
    },
    [run?.runId],
  )

  const onStop = useCallback(() => {
    if (run?.runId) void stopRun(run.runId)
  }, [run?.runId])

  const onCreateProject = useCallback(async (input: { name: string; mode: CodyMode }) => {
    setCreating(true)
    setCreateError(null)
    try {
      const created = await createProject(input)
      setProjects((list) => [...list.filter((p) => p.id !== created.id), created])
      setActiveProjectId(created.id)
      setPage('workspace')
      setNewProjectOpen(false)
    } catch (e) {
      setCreateError(e instanceof Error ? e.message : 'Could not create the project.')
    } finally {
      setCreating(false)
    }
  }, [])

  const onChangeMode = useCallback(
    async (mode: CodyMode) => {
      if (!activeProject || activeProject.id === 'default') return
      setModeBusy(true)
      setModeError(null)
      try {
        const updated = await setProjectMode(activeProject.id, mode)
        setProjects((list) => list.map((p) => (p.id === updated.id ? updated : p)))
      } catch (e) {
        setModeError(e instanceof Error ? e.message : 'Could not change the mode.')
      } finally {
        setModeBusy(false)
      }
    },
    [activeProject],
  )

  const onOpenRun = useCallback(
    (cid: string) => {
      setConvForActive(cid)
      setPage('workspace')
      reload()
    },
    [setConvForActive, reload],
  )

  return (
    <SidebarProvider>
      <CodySidebar
        projects={projects}
        activeProjectId={activeProjectId}
        onSelectProject={(id) => {
          setActiveProjectId(id)
          setPage('workspace')
        }}
        onNewProject={() => setNewProjectOpen(true)}
        page={page}
        onNavigate={setPage}
      />
      <SidebarInset className="bg-card flex h-svh min-h-0 flex-col overflow-hidden">
        <CodyTopbar project={activeProject} run={run} isLive={isLive} onStop={onStop} />
        <main className="min-h-0 flex-1 overflow-hidden">
          {!projectsLoaded ? (
            <CodyLoader variant="ring" label="Loading Cody…" className="h-full justify-center" />
          ) : page === 'workspace' ? (
            <CodyWorkspace
              run={run}
              tier={tier}
              connecting={connection === 'connecting'}
              projectID={
                activeProject && activeProject.id !== 'default' ? activeProject.id : undefined
              }
              mode={activeProject?.mode ?? 'engineer'}
              actions={{
                onStartRun,
                onSteer,
                onAnswer,
                onContinue,
                onRePreview,
                onRestored: reload,
              }}
            />
          ) : page === 'history' ? (
            <CodyHistory
              projectID={activeProjectId ?? undefined}
              cache={recentRuns}
              onOpen={onOpenRun}
            />
          ) : (
            <CodySettings
              project={activeProject}
              onChangeMode={onChangeMode}
              busy={modeBusy}
              error={modeError}
            />
          )}
        </main>
      </SidebarInset>

      <NewProjectDialog
        open={newProjectOpen}
        onOpenChange={setNewProjectOpen}
        onCreate={onCreateProject}
        busy={creating}
        error={createError}
      />
    </SidebarProvider>
  )
}
