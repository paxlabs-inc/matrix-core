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

import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { CodySidebar, type CodyPage } from '@/components/matrix/cody/cody-sidebar'
import { CodyTopbar } from '@/components/matrix/cody/cody-topbar'
import { CodyWorkspace } from '@/components/matrix/cody/cody-workspace'
import { CodyHistory } from '@/components/matrix/cody/cody-history'
import { CodySettings } from '@/components/matrix/cody/cody-settings'
import { NewProjectDialog } from '@/components/matrix/cody/new-project-dialog'
import { CodyLoader } from '@/components/matrix/cody/loaders'
import { useChat } from '@/hooks/api/useChat'
import { createProject, listProjects, type NeoProject } from '@/lib/api/workspace'
import { getPresetBySlug } from '@/lib/data/presets'

export function CodyApp({ initialPreset }: { initialPreset?: string }) {
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
    if (!projectsLoaded || !initialPreset || presetSentRef.current) return
    presetSentRef.current = true
    // Clear the query param so refreshing doesn't re-send.
    const url = new URL(window.location.href)
    url.searchParams.delete('preset')
    window.history.replaceState({}, '', url.toString())
    // Load the full prompt text and send it.
    getPresetBySlug(initialPreset).then((preset) => {
      if (preset) chat.send(preset.prompt)
    })
  }, [projectsLoaded, initialPreset, chat])

  // Switching projects starts a fresh thread scope and re-reads history.
  const selectProject = useCallback(
    (id: string) => {
      if (id !== activeProjectId) chat.reset()
      setActiveProjectId(id)
      setPage('workspace')
    },
    [activeProjectId, chat],
  )
  useEffect(() => {
    if (activeProjectId) chat.refreshConversations()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeProjectId])

  const onCreateProject = useCallback(async (input: { name: string }) => {
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
      chat.selectConversation(convID)
      setPage('workspace')
    },
    [chat],
  )

  return (
    <SidebarProvider>
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
      <SidebarInset className="bg-card flex h-svh min-h-0 flex-col overflow-hidden">
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
            <CodyHistory conversations={chat.conversations} onOpen={onOpenConversation} />
          ) : (
            <CodySettings
              project={activeProject}
              onProjectChanged={onProjectChanged}
              onProjectDeleted={onProjectDeleted}
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
