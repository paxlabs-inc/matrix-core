'use client'

/**
 * Cody sidebar — projects, new project, the Workspace/History/Settings nav, and
 * back to Centra AI. Layers separate by background tone only (the sidebar
 * primitive uses `bg-sidebar`); no border strokes. Icons come from the ported
 * custom set (currentColor, so they track the accent).
 */
import { Button } from '@astryxdesign/core/Button'
import { SideNav, SideNavHeading, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav'
import { DropdownMenu, DropdownMenuItem } from '@astryxdesign/core/DropdownMenu'
import {
  IconArrowLeft,
  IconClock,
  IconFolder,
  IconGrid,
  IconLayers,
  IconMore,
  IconPlus,
  IconSettings,
  IconTrash,
} from '@/components/matrix/cody/icons'
import type { NeoProject } from '@/lib/api/workspace'

export type CodyPage = 'workspace' | 'history' | 'settings'

export type ProjectAction = 'settings' | 'archive' | 'delete'

export function CodySidebar({
  projects,
  archived = [],
  activeProjectId,
  onSelectProject,
  onNewProject,
  onProjectAction,
  page,
  onNavigate,
  backHref = '/',
}: {
  projects: NeoProject[]
  archived?: NeoProject[]
  activeProjectId: string | null
  onSelectProject: (id: string) => void
  onNewProject: () => void
  onProjectAction?: (id: string, action: ProjectAction) => void
  page: CodyPage
  onNavigate: (page: CodyPage) => void
  backHref?: string
}) {
  const nav: { id: CodyPage; label: string; icon: React.ComponentType<{ className?: string }> }[] =
    [
      { id: 'workspace', label: 'Workspace', icon: IconLayers },
      { id: 'history', label: 'History', icon: IconClock },
      { id: 'settings', label: 'Settings', icon: IconSettings },
    ]

  return (
    <SideNav
      header={<SideNavHeading heading="Neo" subheading="Cody Code" />}
      topContent={
        <Button
          label="New project"
          icon={<IconPlus />}
          variant="primary"
          width="100%"
          onClick={onNewProject}
        />
      }
      collapsible={{ buttonLabel: 'Collapse Cody navigation' }}
      footer={
        <>
          <SideNavItem label="Templates" icon={<IconGrid />} href="/code" />
          <SideNavItem label="Back to Centra AI" icon={<IconArrowLeft />} href={backHref} />
        </>
      }
    >
      <SideNavSection title="Projects">
        {projects.map((project) => (
          <SideNavItem
            key={project.id}
            label={project.name}
            icon={<IconFolder />}
            isSelected={project.id === activeProjectId}
            onClick={() => onSelectProject(project.id)}
            endContent={
              onProjectAction && project.id !== 'default' ? (
                <DropdownMenu
                  button={{
                    label: `Manage ${project.name}`,
                    icon: <IconMore />,
                    variant: 'ghost',
                    size: 'sm',
                    isIconOnly: true,
                  }}
                  placement="end"
                  menuWidth={176}
                >
                  <DropdownMenuItem
                    label="Rename…"
                    icon={<IconSettings />}
                    onClick={() => onProjectAction(project.id, 'settings')}
                  />
                  <DropdownMenuItem
                    label="Archive"
                    icon={<IconClock />}
                    onClick={() => onProjectAction(project.id, 'archive')}
                  />
                  <DropdownMenuItem
                    label="Delete…"
                    icon={<IconTrash />}
                    onClick={() => onProjectAction(project.id, 'delete')}
                  />
                </DropdownMenu>
              ) : undefined
            }
          />
        ))}
        {projects.length === 0 ? (
          <SideNavItem label="New project" icon={<IconPlus />} onClick={onNewProject} />
        ) : null}
      </SideNavSection>

      {archived.length > 0 ? (
        <SideNavSection title="Archived">
          {archived.map((project) => (
            <SideNavItem
              key={project.id}
              label={project.name}
              icon={<IconFolder />}
              endContent="Restore"
              onClick={() => onProjectAction?.(project.id, 'archive')}
            />
          ))}
        </SideNavSection>
      ) : null}

      <SideNavSection title="View">
        {nav.map((item) => (
          <SideNavItem
            key={item.id}
            label={item.label}
            icon={<item.icon />}
            isSelected={page === item.id}
            onClick={() => onNavigate(item.id)}
          />
        ))}
      </SideNavSection>
    </SideNav>
  )
}
