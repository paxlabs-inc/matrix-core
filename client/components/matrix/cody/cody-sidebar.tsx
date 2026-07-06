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
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from '@/components/ui/sidebar'
import {
  IconArrowLeft,
  IconClock,
  IconFolder,
  IconLayers,
  IconPlus,
  IconSettings,
} from '@/components/matrix/cody/icons'
import { MODE_LABELS } from '@/components/matrix/cody/tiering'
import type { CodyProject } from '@/lib/api/cody'

export type CodyPage = 'workspace' | 'history' | 'settings'

export function CodySidebar({
  projects,
  activeProjectId,
  onSelectProject,
  onNewProject,
  page,
  onNavigate,
  backHref = '/',
}: {
  projects: CodyProject[]
  activeProjectId: string | null
  onSelectProject: (id: string) => void
  onNewProject: () => void
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
          <span className="font-mono text-sm font-semibold tracking-tight">Cody</span>
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
                    tooltip={`${p.name} · ${MODE_LABELS[p.mode]}`}
                    onClick={() => onSelectProject(p.id)}
                  >
                    <IconFolder />
                    <span className="truncate">{p.name}</span>
                    <span className="text-muted-foreground ml-auto font-mono text-[10px] uppercase">
                      {p.mode.slice(0, 4)}
                    </span>
                  </SidebarMenuButton>
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
