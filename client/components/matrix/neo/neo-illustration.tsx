'use client'

/**
 * NeoIllustration — the branded empty-state artwork for Neo's pages.
 *
 * Renders the polychrome illustrations the user placed in `icons/neo/` (imported
 * as static assets and served through next/image, which passes SVG through
 * untouched). Used as the centerpiece of an empty Timeline / Workspace / Files
 * page so a blank state still feels like part of the product, not a void.
 *
 * Design system: the art carries its own palette; the wrapper adds no border,
 * shadow, or glow — separation elsewhere is by background tone only.
 */
import Image, { type StaticImageData } from 'next/image'
import timelineArt from '@/icons/neo/timeline.svg'
import workspaceArt from '@/icons/neo/workspace.svg'
import filesArt from '@/icons/neo/files.svg'
import agentsArt from '@/icons/neo/agents.svg'
import { cn } from '@/lib/utils'

export type NeoArt = 'timeline' | 'workspace' | 'files' | 'agents'

const ART: Record<NeoArt, StaticImageData> = {
  timeline: timelineArt,
  workspace: workspaceArt,
  files: filesArt,
  agents: agentsArt,
}

export function NeoIllustration({
  art,
  className,
  width = 200,
}: {
  art: NeoArt
  className?: string
  /** Rendered width in px; height follows the intrinsic aspect ratio. */
  width?: number
}) {
  const src = ART[art]
  return (
    <Image
      src={src}
      alt=""
      aria-hidden
      width={width}
      height={Math.round((width * src.height) / src.width)}
      className={cn('select-none', className)}
      priority={false}
    />
  )
}
