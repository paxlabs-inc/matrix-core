import * as React from 'react'
import * as AppsIcon from '@openai/apps-sdk-ui/components/Icon'

type Glyph = React.ComponentType<React.ComponentPropsWithRef<'svg'>>

export interface IconProps extends React.SVGProps<SVGSVGElement> {
  size?: number | string
}

function icon(name: string, GlyphComponent: Glyph) {
  return React.forwardRef<SVGSVGElement, IconProps>(function KeithIcon(
    { size = 20, color = 'currentColor', 'aria-hidden': hidden, 'aria-label': label, ...props },
    ref,
  ) {
    return (
      <GlyphComponent
        ref={ref}
        width={size}
        height={size}
        color={color}
        aria-hidden={hidden ?? (label ? undefined : true)}
        aria-label={label}
        data-keith-icon={name}
        {...props}
      />
    )
  })
}

export const Activity = icon('activity', AppsIcon.Pulse)
export const Agent = icon('agent', AppsIcon.Agent)
export const Archive = icon('archive', AppsIcon.Archive)
export const ArrowLeft = icon('arrow-left', AppsIcon.ArrowLeft)
export const ArrowUp = icon('arrow-up', AppsIcon.ArrowUp)
export const Brain = icon('brain', AppsIcon.Brain)
export const Calendar = icon('calendar', AppsIcon.Calendar)
export const Chat = icon('chat', AppsIcon.Chat)
export const Check = icon('check', AppsIcon.Check)
export const CheckCircle = icon('success', AppsIcon.CheckCircle)
export const ChevronDown = icon('chevron-down', AppsIcon.ChevronDown)
export const Clock = icon('clock', AppsIcon.Clock)
export const Code = icon('code', AppsIcon.Code)
export const Copy = icon('copy', AppsIcon.Copy)
export const Download = icon('download', AppsIcon.Download)
export const ExportIcon = icon('external-link', AppsIcon.ExternalLink)
export const File = icon('file', AppsIcon.FileDocument)
export const Goal = icon('goal', AppsIcon.Status)
export const Memory = icon('memory', AppsIcon.Storage)
export const Menu = icon('menu', AppsIcon.Menu)
export const Monitor = icon('monitor', AppsIcon.Desktop)
export const More = icon('more', AppsIcon.DotsHorizontal)
export const Plus = icon('plus', AppsIcon.Plus)
export const Refresh = icon('refresh', AppsIcon.ArrowRotateCcw)
export const Search = icon('search', AppsIcon.Search)
export const Settings = icon('settings', AppsIcon.Settings)
export const Stop = icon('stop', AppsIcon.Stop)
export const Tools = icon('tools', AppsIcon.Tools)
export const User = icon('user', AppsIcon.User)
export const Warning = icon('warning', AppsIcon.TriangleExclamationErrorWarning)
export const X = icon('close', AppsIcon.X)
