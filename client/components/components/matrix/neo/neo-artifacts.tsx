'use client'

/**
 * NeoArtifacts — in-app delivery of deliverable artifacts (F6).
 *
 * When Neo produces a downloadable file/archive or deploys a site, the daemon
 * emits a `tool.artifact` event (engine.surfaceTool) instead of leaving a raw
 * bucket URL buried in the answer text. This renders each one as a first-class
 * card: a download card for files/archives and an open-and-preview card for a
 * deployed site. The url is a content-addressed / public ref — we link to it
 * directly (no inline bytes ever live in the thread).
 *
 * Design system: separation by background TONE only (bg-card / bg-muted), no
 * border strokes for depth, single accent via text-primary, no emojis/glow.
 */
import { Download, ExternalLink, FileText, Globe, Package } from '@/lib/matrix-icons'
import type { ChatArtifact } from '@/hooks/api/useChat'

function formatBytes(size?: number): string {
  if (!size || size <= 0) return ''
  const units = ['B', 'KB', 'MB', 'GB']
  let n = size
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`
}

function fileName(a: ChatArtifact): string {
  if (a.name) return a.name
  try {
    const u = new URL(a.url, 'https://x')
    const last = u.pathname.split('/').filter(Boolean).pop()
    if (last) return decodeURIComponent(last)
  } catch {
    /* not a parseable URL — fall through */
  }
  return a.kind === 'site' ? 'Deployed site' : 'Download'
}

function ArtifactCard({ artifact }: { artifact: ChatArtifact }) {
  const isSite = artifact.kind === 'site'
  const Icon = isSite ? Globe : artifact.kind === 'archive' ? Package : FileText
  const meta = [artifact.kind === 'archive' ? 'Archive' : artifact.mime, formatBytes(artifact.size)]
    .filter(Boolean)
    .join(' · ')

  return (
    <div className="bg-card flex flex-col gap-3 rounded-xl p-3">
      {isSite && artifact.preview && (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={artifact.preview}
          alt={fileName(artifact)}
          className="bg-muted h-36 w-full rounded-lg object-cover object-top"
        />
      )}
      <div className="flex items-center gap-3">
        <span className="bg-muted text-primary flex size-9 shrink-0 items-center justify-center rounded-lg">
          <Icon className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-foreground truncate text-sm font-medium">{fileName(artifact)}</p>
          {(meta || isSite) && (
            <p className="text-muted-foreground truncate text-xs">{isSite ? artifact.url : meta}</p>
          )}
        </div>
        <a
          href={artifact.url}
          target={isSite ? '_blank' : undefined}
          rel={isSite ? 'noopener noreferrer' : undefined}
          download={isSite ? undefined : fileName(artifact)}
          className="bg-primary text-primary-foreground inline-flex shrink-0 items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium transition-opacity hover:opacity-90"
        >
          {isSite ? (
            <>
              <ExternalLink className="size-3.5" />
              Open
            </>
          ) : (
            <>
              <Download className="size-3.5" />
              Download
            </>
          )}
        </a>
      </div>
    </div>
  )
}

/** Render the run's deliverable artifacts (from tool.artifact events). */
export function NeoArtifacts({ artifacts }: { artifacts?: ChatArtifact[] }) {
  if (!artifacts || artifacts.length === 0) return null
  return (
    <div className="flex flex-col gap-2">
      {artifacts.map((a, i) => (
        <ArtifactCard key={`${a.url}-${i}`} artifact={a} />
      ))}
    </div>
  )
}
