'use client'

/**
 * Neo media rendering — images / video / audio inside the Adaptive Surface.
 *
 * Generated and uploaded media live on the agent's own machine volume and are
 * served behind per-user auth from `/media/<name>`. A raw <img>/<video> src
 * cannot carry the bearer, so `NeoMediaItem` fetches the bytes as an authed
 * blob (via `loadMediaObjectURL`) and renders an object URL, revoking it on
 * unmount. `parseAttachments` pulls the composer's `[attached …: /media/…]`
 * markers out of a user turn so the upload renders as a thumbnail and the
 * marker never shows up as literal text in the bubble.
 */
import { useEffect, useState } from 'react'
import { useTranslations } from 'next-intl'
import { loadMediaObjectURL, type MediaKind } from '@/lib/api/media'
import type { ChatMedia } from '@/hooks/api/useChat'

export interface ParsedMedia {
  url: string
  kind: MediaKind
  prompt?: string
}

// Matches the markers the composer embeds for uploaded files, e.g.
// "[attached image: /media/2026..ab.png]".
const ATTACH_RE = /\[attached (image|video|audio):\s*(\/media\/[^\]\s]+)\]/g

/** Split a user turn into visible text + the media it carried. */
export function parseAttachments(text: string): { clean: string; items: ParsedMedia[] } {
  const items: ParsedMedia[] = []
  ATTACH_RE.lastIndex = 0
  let m: RegExpExecArray | null
  while ((m = ATTACH_RE.exec(text)) !== null) {
    items.push({ url: m[2], kind: m[1] as MediaKind })
  }
  const clean = text
    .replace(ATTACH_RE, '')
    .replace(/\n{3,}/g, '\n\n')
    .trim()
  return { clean, items }
}

/** Render a single `/media/<name>` reference behind per-user auth. */
export function NeoMediaItem({ url, kind, prompt }: ParsedMedia) {
  const t = useTranslations('agentChat')
  const [src, setSrc] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    let obj: string | null = null
    setSrc(null)
    setFailed(false)
    loadMediaObjectURL(url).then((o) => {
      if (!active) {
        if (o) URL.revokeObjectURL(o)
        return
      }
      if (o) {
        obj = o
        setSrc(o)
      } else {
        setFailed(true)
      }
    })
    return () => {
      active = false
      if (obj) URL.revokeObjectURL(obj)
    }
  }, [url])

  if (failed) {
    return <span className="text-muted-foreground text-xs">{t('mediaUnavailable')}</span>
  }
  if (!src) {
    return <div className="bg-muted h-44 w-full max-w-sm animate-pulse rounded-xl" aria-hidden />
  }
  if (kind === 'video') {
    return (
      <video
        src={src}
        controls
        className="max-h-[26rem] w-full max-w-md rounded-xl"
        preload="metadata"
      />
    )
  }
  if (kind === 'audio') {
    return <audio src={src} controls className="w-full max-w-md" preload="metadata" />
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img
      src={src}
      alt={prompt || t('mediaImageAlt')}
      className="max-h-[26rem] w-full max-w-md rounded-xl object-contain"
    />
  )
}

/** Render a list of generated media artifacts (from a tool.media event). */
export function NeoMediaGrid({ media }: { media?: ChatMedia[] }) {
  if (!media || media.length === 0) return null
  return (
    <div className="mt-2 flex flex-col gap-2">
      {media.map((mi, i) => (
        <NeoMediaItem key={`${mi.url}-${i}`} url={mi.url} kind={mi.kind} prompt={mi.prompt} />
      ))}
    </div>
  )
}
