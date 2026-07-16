'use client'

/**
 * Cody sidebar — projects, new project, the Workspace/History/Settings nav, and
 * back-to-Matrix. Layers separate by background tone only (the sidebar
 * primitive uses `bg-sidebar`); no border strokes. Icons come from the ported
 * custom set (currentColor, so they track the accent).
 */
import { Link } from '@/i18n/navigation'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupAction,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuAction,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from '@/components/ui/sidebar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
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
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex h-12 items-center px-2">
          <span className="font-mono text-sm font-semibold tracking-tight">Neo</span>
        </div>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Projects</SidebarGroupLabel>
          <SidebarGroupAction title="New project" onClick={onNewProject}>
            <IconPlus />
            <span className="sr-only">New project</span>
          </SidebarGroupAction>
          <SidebarGroupContent>
            <SidebarMenu>
              {projects.map((p) => (
                <SidebarMenuItem key={p.id}>
                  <SidebarMenuButton
                    isActive={p.id === activeProjectId}
                    tooltip={p.name}
                    onClick={() => onSelectProject(p.id)}
                  >
                    <IconFolder />
                    <span className="truncate">{p.name}</span>
                  </SidebarMenuButton>
                  {onProjectAction && p.id !== 'default' ? (
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <SidebarMenuAction showOnHover>
                          <IconMore />
                          <span className="sr-only">Project actions</span>
                        </SidebarMenuAction>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent side="right" align="start">
                        <DropdownMenuItem onClick={() => onProjectAction(p.id, 'settings')}>
                          <IconSettings />
                          <span>Rename…</span>
                        </DropdownMenuItem>
                        <DropdownMenuItem onClick={() => onProjectAction(p.id, 'archive')}>
                          <IconClock />
                          <span>Archive</span>
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          variant="destructive"
                          onClick={() => onProjectAction(p.id, 'delete')}
                        >
                          <IconTrash />
                          <span>Delete…</span>
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  ) : null}
                </SidebarMenuItem>
              ))}
              {projects.length === 0 ? (
                <SidebarMenuItem>
                  <SidebarMenuButton onClick={onNewProject}>
                    <IconPlus />
                    <span>New project</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ) : null}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        {archived.length > 0 ? (
          <SidebarGroup>
            <SidebarGroupLabel>Archived</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {archived.map((p) => (
                  <SidebarMenuItem key={p.id}>
                    <SidebarMenuButton
                      tooltip={`${p.name} · archived`}
                      className="opacity-60"
                      onClick={() => onProjectAction?.(p.id, 'archive')}
                    >
                      <IconFolder />
                      <span className="truncate">{p.name}</span>
                      <span className="text-muted-foreground ml-auto font-mono text-[10px] uppercase">
                        restore
                      </span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ) : null}

        <SidebarGroup>
          <SidebarGroupLabel>View</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {nav.map((item) => (
                <SidebarMenuItem key={item.id}>
                  <SidebarMenuButton
                    isActive={page === item.id}
                    tooltip={item.label}
                    onClick={() => onNavigate(item.id)}
                  >
                    <item.icon />
                    <span>{item.label}</span>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild tooltip="Templates">
              <Link href="/code">
                <IconGrid />
                <span>Templates</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton asChild tooltip="Back to Matrix">
              <Link href={backHref}>
                <IconArrowLeft />
                <span>Back to Matrix</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
  )
}
