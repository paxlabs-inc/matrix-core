'use client'

/**
 * Cody media — image/video/audio upload + rendering, mirroring Neo's proven
 * plane (agent-chat.tsx markers + neo-media.tsx authed blob rendering).
 *
 * Uploads land on the user's machine volume via POST /cody/upload; the
 * composer embeds `[attached image: /media/…]` markers in the message so the
 * reference survives in the durable transcript. `parseCodyAttachments` splits
 * a user turn back into text + media; `CodyMediaItem` fetches the bytes as an
 * authed blob (an <img> src cannot carry the bearer) and renders an object
 * URL, revoking it on unmount.
 */
import { useEffect, useRef, useState } from 'react'

import { CodyLoader } from '@/components/matrix/cody/loaders'
import { IconPaperclip, IconClose } from '@/components/matrix/cody/icons'
import {
  loadCodyMediaObjectURL,
  uploadCodyMedia,
  type CodyMediaKind,
  type CodyUploadedMedia,
} from '@/lib/api/cody'
import { cn } from '@/lib/utils'

export interface CodyParsedMedia {
  url: string
  kind: CodyMediaKind
}

// The same marker family Neo's composer embeds for uploads.
const ATTACH_RE = /\[attached (image|video|audio):\s*(\/media\/[^\]\s]+)\]/g

/** Split a user turn into visible text + the media it carried. */
export function parseCodyAttachments(text: string): { clean: string; items: CodyParsedMedia[] } {
  const items: CodyParsedMedia[] = []
  ATTACH_RE.lastIndex = 0
  let m: RegExpExecArray | null
  while ((m = ATTACH_RE.exec(text)) !== null) {
    items.push({ url: m[2], kind: m[1] as CodyMediaKind })
  }
  const clean = text
    .replace(ATTACH_RE, '')
    .replace(/\n{3,}/g, '\n\n')
    .trim()
  return { clean, items }
}

/** The marker a composer embeds for one uploaded attachment. */
export function attachmentMarker(up: CodyUploadedMedia): string {
  return `[attached ${up.kind}: ${up.url}]`
}

/** Render one `/media/<name>` reference behind per-user auth. */
export function CodyMediaItem({ url, kind }: CodyParsedMedia) {
  const [src, setSrc] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    let obj: string | null = null
    setSrc(null)
    setFailed(false)
    // oxlint-disable-next-line eslint-plugin-promise(prefer-await-to-then)
    void loadCodyMediaObjectURL(url).then((o) => {
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
    return <span className="text-muted-foreground text-xs">Media unavailable.</span>
  }
  if (!src) {
    return <div className="bg-surface-tertiary h-24 w-full max-w-[180px] animate-pulse rounded-md" aria-hidden />
  }
  if (kind === 'video') {
    return <video src={src} controls className="max-h-48 w-full max-w-xs rounded-md" preload="metadata" />
  }
  if (kind === 'audio') {
    return <audio src={src} controls className="w-full max-w-xs" preload="metadata" />
  }
  return (
    // eslint-disable-next-line @next/next/no-img-element
    <img src={src} alt="attachment" className="max-h-48 w-full max-w-xs rounded-md object-contain" />
  )
}

/** Thumbnails for the media a user turn carried. */
export function CodyAttachmentList({ items }: { items: CodyParsedMedia[] }) {
  if (items.length === 0) return null
  return (
    <div className="flex flex-wrap gap-2">
      {items.map((it, i) => (
        <CodyMediaItem key={`${it.url}-${i}`} url={it.url} kind={it.kind} />
      ))}
    </div>
  )
}

/**
 * The composer attach control: pick files, upload them to the machine volume,
 * and hand the stored references up. Pending uploads show inline and can be
 * removed before sending.
 */
export function CodyAttach({
  attachments,
  onChange,
  className,
}: {
  attachments: CodyUploadedMedia[]
  onChange: (items: CodyUploadedMedia[]) => void
  className?: string
}) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const pick = async (files: FileList | null) => {
    if (!files || files.length === 0) return
    setBusy(true)
    setError(null)
    try {
      const uploaded: CodyUploadedMedia[] = []
      for (const f of Array.from(files)) {
        uploaded.push(await uploadCodyMedia(f, f.name))
      }
      onChange([...attachments, ...uploaded])
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Upload failed.')
    } finally {
      setBusy(false)
      if (inputRef.current) inputRef.current.value = ''
    }
  }

  return (
    <div className={cn('flex min-w-0 items-center gap-2', className)}>
      <input
        ref={inputRef}
        type="file"
        accept="image/*,video/*,audio/*"
        multiple
        className="hidden"
        onChange={(e) => void pick(e.target.files)}
      />
      <button
        type="button"
        title="Attach an image"
        onClick={() => inputRef.current?.click()}
        disabled={busy}
        className="text-muted-foreground hover:text-foreground hover:bg-surface-hover flex size-7 shrink-0 items-center justify-center rounded-md transition-colors disabled:opacity-60"
      >
        {busy ? <CodyLoader variant="ring" size={14} /> : <IconPaperclip className="size-4" />}
      </button>
      {attachments.length > 0 ? (
        <div className="flex min-w-0 flex-wrap items-center gap-1">
          {attachments.map((a) => (
            <span
              key={a.url}
              className="bg-surface-hover flex max-w-40 items-center gap-1 rounded px-1.5 py-0.5 font-mono text-[10px]"
            >
              <span className="truncate">{a.name}</span>
              <button
                type="button"
                title="Remove"
                onClick={() => onChange(attachments.filter((x) => x.url !== a.url))}
                className="text-muted-foreground hover:text-foreground shrink-0"
              >
                <IconClose className="size-3" />
              </button>
            </span>
          ))}
        </div>
      ) : null}
      {error ? <span className="text-destructive truncate text-[11px]">{error}</span> : null}
    </div>
  )
}
