'use client'

/**
 * Canvas — a show-as-is visual/media blob with optional interactive regions:
 * a rendered web page, image, chart, audio, video. Regions are trusted
 * hotspots the agent placed (percent-positioned) that fire an Ask when tapped
 * — the spatial bridge into the back-channel.
 */
import { Globe, ImageIcon } from '@/lib/matrix-icons'
import type { Canvas as CanvasPayload, CanvasRegion } from '@/lib/construct/types.gen'
import { ConstructChart } from '@/components/centra/packages/construct/chart'

function Regions({
  regions,
  onAsk,
}: {
  regions: CanvasRegion[]
  onAsk?: (askRef: string) => void
}) {
  return (
    <>
      {regions.map((r) => (
        <button
          key={r.id}
          type="button"
          onClick={() => r.ask_ref && onAsk?.(r.ask_ref)}
          title={r.label}
          className="bg-primary/15 hover:bg-primary/25 absolute rounded-md transition-colors"
          style={{ left: `${r.x}%`, top: `${r.y}%`, width: `${r.w}%`, height: `${r.h}%` }}
        >
          {r.label && (
            <span className="text-primary-foreground bg-primary/80 absolute -top-5 left-0 rounded px-1 py-0.5 text-[0.6rem] whitespace-nowrap">
              {r.label}
            </span>
          )}
        </button>
      ))}
    </>
  )
}

export function CanvasView({
  canvas,
  onAsk,
}: {
  canvas: CanvasPayload
  onAsk?: (askRef: string) => void
}) {
  const { media, caption, regions } = canvas
  const hasRegions = regions && regions.length > 0

  // Data-driven chart: render the exact GAIA chart component standalone (it
  // brings its own card chrome), fed by the inline chart data.
  if (media.kind === 'chart' && canvas.chart) {
    return <ConstructChart chart={canvas.chart} title={caption} />
  }

  let body: React.ReactNode
  if (media.kind === 'image' || media.kind === 'chart') {
    body = media.url ? (
      <div className="relative">
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img
          src={media.url}
          alt={media.alt || caption || ''}
          className="block max-h-96 w-full object-contain"
        />
        {hasRegions && <Regions regions={regions} onAsk={onAsk} />}
      </div>
    ) : (
      <Placeholder icon="image" label={caption || 'image'} />
    )
  } else if (media.kind === 'video') {
    body = media.url ? (
      <video src={media.url} controls className="block max-h-96 w-full bg-black" />
    ) : (
      <Placeholder icon="image" label={caption || 'video'} />
    )
  } else if (media.kind === 'audio') {
    body = media.url ? (
      <div className="px-4 py-4">
        <audio src={media.url} controls className="w-full" />
      </div>
    ) : (
      <Placeholder icon="image" label={caption || 'audio'} />
    )
  } else {
    // page: a captured/rendered web page — frame it as a page card with a
    // live-open affordance (we never embed arbitrary cross-origin frames).
    body = (
      <a
        href={media.url || '#'}
        target="_blank"
        rel="noreferrer noopener"
        className="hover:bg-foreground/[0.04] flex items-center gap-3 px-4 py-3.5 transition-colors"
      >
        <span className="bg-primary/15 text-primary grid size-9 shrink-0 place-items-center rounded-xl">
          <Globe className="size-[1.1rem]" />
        </span>
        <span className="min-w-0">
          <span className="text-foreground line-clamp-1 block text-sm font-medium">
            {media.alt || caption || media.url || 'page'}
          </span>
          {media.url && (
            <span className="text-muted-foreground/80 line-clamp-1 block font-mono text-[0.68rem]">
              {media.url}
            </span>
          )}
        </span>
      </a>
    )
  }

  return (
    <div className="bg-background overflow-hidden rounded-xl">
      {body}
      {caption && media.kind !== 'page' && (
        <p className="text-muted-foreground/85 px-3.5 py-2.5 text-xs leading-snug">{caption}</p>
      )}
    </div>
  )
}

function Placeholder({ icon, label }: { icon: 'image' | 'globe'; label: string }) {
  const Icon = icon === 'globe' ? Globe : ImageIcon
  return (
    <div className="text-muted-foreground/70 flex flex-col items-center justify-center gap-2 px-4 py-10 text-center">
      <Icon className="size-6" />
      <p className="text-xs">{label}</p>
    </div>
  )
}
