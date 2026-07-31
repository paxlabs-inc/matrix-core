'use client'

/**
 * The Neo coding app (NEO-WORKBENCH req 1, 8) — the /cody route rebranded to
 * Neo end to end. Owns the shell (sidebar + top bar + the ONE workbench
 * layout), the project registry, and the per-project Neo conversation.
 *
 * Coding runs ARE Neo conversations: the chat rail folds the same useChat
 * reducer and event stream the dashboard uses (no second reducer, no codyd).
 * History is server-backed and project-scoped.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Layout } from '@astryxdesign/core/Layout'

import { CodySidebar, type CodyPage } from '@/components/matrix/cody/cody-sidebar'
import { CodyTopbar } from '@/components/matrix/cody/cody-topbar'
import { CodyWorkspace } from '@/components/matrix/cody/cody-workspace'
import { CodyHistory } from '@/components/matrix/cody/cody-history'
import { CodySettings } from '@/components/matrix/cody/cody-settings'
import { NewProjectDialog } from '@/components/matrix/cody/new-project-dialog'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { useChat } from '@/hooks/api/useChat'
import { createProject, listProjects, type NeoProject } from '@/lib/api/workspace'

const CONVERSATION_QUERY = 'conversation'
const PROJECT_QUERY = 'project'

function conversationFromLocation(): string | null {
  return new URL(window.location.href).searchParams.get(CONVERSATION_QUERY)
}

function projectFromLocation(): string | null {
  return new URL(window.location.href).searchParams.get(PROJECT_QUERY)
}

function writeConversationLocation(
  id: string | null,
  mode: 'push' | 'replace',
  project?: string | null,
) {
  const url = new URL(window.location.href)
  if (id) url.searchParams.set(CONVERSATION_QUERY, id)
  else url.searchParams.delete(CONVERSATION_QUERY)
  if (project) url.searchParams.set(PROJECT_QUERY, project)
  else url.searchParams.delete(PROJECT_QUERY)
  window.history[mode === 'push' ? 'pushState' : 'replaceState'](
    window.history.state,
    '',
    url.toString(),
  )
}

export function CodyApp({ initialPresetPrompt }: { initialPresetPrompt?: string }) {
  const [projects, setProjects] = useState<NeoProject[]>([])
  const [projectsLoaded, setProjectsLoaded] = useState(false)
  const [activeProjectId, setActiveProjectId] = useState<string | null>(null)
  const [page, setPage] = useState<CodyPage>('workspace')

  const [newProjectOpen, setNewProjectOpen] = useState(false)
  const [creating, setCreating] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)

  const activeProject = useMemo(
    () => projects.find((p) => p.id === activeProjectId) ?? null,
    [projects, activeProjectId],
  )

  // ONE reducer: the same Neo conversation model the dashboard uses, scoped
  // to the active project (tagged sends + filtered history).
  const chat = useChat({ project: activeProjectId ?? undefined })
  const activeConversationRef = useRef(chat.conversationId)
  const selectConversationRef = useRef(chat.selectConversation)
  const resetConversationRef = useRef(chat.reset)
  activeConversationRef.current = chat.conversationId
  selectConversationRef.current = chat.selectConversation
  resetConversationRef.current = chat.reset

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

  // Auto-send a preset prompt when arriving from the /code gallery.
  const presetSentRef = useRef(false)
  useEffect(() => {
    if (!projectsLoaded || !initialPresetPrompt || presetSentRef.current) return
    presetSentRef.current = true
    // Clear the query param so refreshing doesn't re-send.
    const url = new URL(window.location.href)
    url.searchParams.delete('preset')
    window.history.replaceState({}, '', url.toString())
    chat.send(initialPresetPrompt)
  }, [projectsLoaded, initialPresetPrompt, chat])

  // Switching projects starts a fresh thread scope and re-reads history.
  const selectProject = useCallback(
    (id: string) => {
      if (id !== activeProjectId) {
        writeConversationLocation(null, 'push', id)
        chat.reset()
      }
      setActiveProjectId(id)
      setPage('workspace')
    },
    [activeProjectId, chat],
  )
  useEffect(() => {
    if (activeProjectId) chat.refreshConversations()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeProjectId])

  // A selected task is browser state as well as reducer state. Deep links,
  // refresh, Back, and Forward must all reopen the exact durable discussion.
  useEffect(() => {
    if (!projectsLoaded) return
    const restoreLocation = () => {
      const projectID = projectFromLocation()
      const conversationID = conversationFromLocation()
      if (projectID) setActiveProjectId(projectID)
      if (conversationID) {
        selectConversationRef.current(conversationID)
        setPage('workspace')
      } else if (activeConversationRef.current) {
        resetConversationRef.current()
      }
    }
    restoreLocation()
    window.addEventListener('popstate', restoreLocation)
    return () => window.removeEventListener('popstate', restoreLocation)
  }, [projectsLoaded])

  // New conversations receive their durable id after submit. Record it in the
  // current history entry so a hard refresh resumes the same task.
  useEffect(() => {
    if (chat.conversationId && conversationFromLocation() !== chat.conversationId) {
      writeConversationLocation(chat.conversationId, 'replace', activeProjectId)
    }
  }, [activeProjectId, chat.conversationId])

  const onCreateProject = useCallback(
    async (input: { name: string }) => {
      setCreating(true)
      setCreateError(null)
      try {
        const created = await createProject(input)
        setProjects((list) => [...list.filter((p) => p.id !== created.id), created])
        writeConversationLocation(null, 'push', created.id)
        chat.reset()
        setActiveProjectId(created.id)
        setPage('workspace')
        setNewProjectOpen(false)
      } catch (e) {
        setCreateError(e instanceof Error ? e.message : 'Could not create the project.')
      } finally {
        setCreating(false)
      }
    },
    [chat],
  )

  const onProjectChanged = useCallback((updated: NeoProject) => {
    setProjects((list) => list.map((p) => (p.id === updated.id ? updated : p)))
  }, [])

  const onProjectDeleted = useCallback((id: string) => {
    setProjects((list) => list.filter((p) => p.id !== id))
    setActiveProjectId((cur) => (cur === id ? 'default' : cur))
    setPage('workspace')
  }, [])

  const onProjectAction = useCallback(
    (id: string, action: 'settings' | 'archive' | 'delete') => {
      if (action === 'archive') {
        const target = projects.find((p) => p.id === id)
        if (!target || target.id === 'default') return
        import('@/lib/api/workspace').then(({ updateProject }) =>
          updateProject(id, { archived: !target.archived })
            .then(onProjectChanged)
            .catch(() => {}),
        )
        if (activeProjectId === id && !target.archived) setActiveProjectId('default')
        return
      }
      setActiveProjectId(id)
      setPage('settings')
    },
    [projects, activeProjectId, onProjectChanged],
  )

  const visibleProjects = useMemo(() => projects.filter((p) => !p.archived), [projects])

  const onOpenConversation = useCallback(
    (convID: string) => {
      writeConversationLocation(convID, 'push', activeProjectId)
      chat.selectConversation(convID)
      setPage('workspace')
    },
    [activeProjectId, chat],
  )

  return (
    <Layout
      height="fill"
      padding={0}
      className="h-svh"
      start={
        <CodySidebar
          projects={visibleProjects}
          archived={projects.filter((p) => p.archived)}
          activeProjectId={activeProjectId}
          onProjectAction={onProjectAction}
          onSelectProject={selectProject}
          onNewProject={() => setNewProjectOpen(true)}
          page={page}
          onNavigate={setPage}
        />
      }
      content={
        <>
          <div className="bg-card flex h-svh min-h-0 flex-col overflow-hidden">
            <CodyTopbar project={activeProject} phase={chat.phase} onStop={chat.dismissTask} />
            <main className="flex min-h-0 flex-1 flex-col overflow-hidden">
              {!projectsLoaded ? (
                <CodyLoader variant="ring" label="Loading…" className="h-full justify-center" />
              ) : page === 'workspace' ? (
                <CodyWorkspace
                  chat={chat}
                  project={activeProject?.id}
                  projectName={activeProject?.name}
                />
              ) : page === 'history' ? (
                <CodyHistory
                  conversations={chat.conversations}
                  onOpen={onOpenConversation}
                  onRename={chat.renameConversation}
                  onArchive={chat.archiveConversation}
                  onDelete={chat.deleteConversation}
                />
              ) : (
                <CodySettings
                  project={activeProject}
                  onProjectChanged={onProjectChanged}
                  onProjectDeleted={onProjectDeleted}
                />
              )}
            </main>
          </div>

          <NewProjectDialog
            open={newProjectOpen}
            onOpenChange={setNewProjectOpen}
            onCreate={onCreateProject}
            busy={creating}
            error={createError}
          />
        </>
      }
    />
  )
}
