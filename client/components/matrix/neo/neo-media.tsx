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
 *
 * Post-generation surface (images only): after a generate/edit lands, the user
 * can (1) describe a targeted change and send it back to Neo with the media
 * ref, (2) request variations, and (3) pick smart prompt recommendations
 * derived from the original generation prompt.
 */
import { useEffect, useMemo, useState } from 'react'
import { useTranslations } from 'next-intl'
import {
  Download,
  FileText,
  Lightbulb,
  RotateCcw,
  Send,
  SlidersHorizontal,
  X,
} from '@/lib/matrix-icons'
import { downloadMediaRef, loadMediaObjectURL, type MediaKind } from '@/lib/api/media'
import type { ChatMedia } from '@/hooks/api/useChat'
import { cn } from '@/lib/utils'
import {
  AudioPlayer,
  AudioPlayerControlBar,
  AudioPlayerDurationDisplay,
  AudioPlayerElement,
  AudioPlayerMuteButton,
  AudioPlayerPlayButton,
  AudioPlayerTimeDisplay,
  AudioPlayerTimeRange,
  AudioPlayerVolumeRange,
} from '@/components/ai-elements/audio-player'

export interface ParsedMedia {
  url: string
  kind: MediaKind
  prompt?: string
  /** Original filename, carried by document markers so the chip is readable. */
  name?: string
}

/** Callback the surface wires to `send` so post-gen actions become real turns. */
export type MediaActionHandler = (instruction: string) => void

// Matches the markers the composer embeds for uploaded files, e.g.
// "[attached image: /media/2026..ab.png]" or
// "[attached file: /media/2026..ab.pdf (report.pdf)]".
const ATTACH_RE = /\[attached (image|video|audio|file):\s*(\/media\/[^\]\s]+)(?:\s+\(([^)]+)\))?\]/g

/** Split a user turn into visible text + the media it carried. */
export function parseAttachments(text: string): { clean: string; items: ParsedMedia[] } {
  const items: ParsedMedia[] = []
  ATTACH_RE.lastIndex = 0
  let m: RegExpExecArray | null
  while ((m = ATTACH_RE.exec(text)) !== null) {
    items.push({ url: m[2], kind: m[1] as MediaKind, name: m[3] || undefined })
  }
  const clean = text
    .replace(ATTACH_RE, '')
    .replace(/\n{3,}/g, '\n\n')
    .trim()
  return { clean, items }
}

/** Derive a sensible download filename from the `/media/<name>` reference. */
function mediaFileName(url: string, kind: MediaKind): string {
  try {
    const last = new URL(url, 'https://x').pathname.split('/').pop()
    if (last) return decodeURIComponent(last)
  } catch {
    /* fall through to the generic name */
  }
  return kind
}

/** Corner download affordance over a rendered media item. The src is already
 *  an authed blob object URL, so the download works without another fetch. */
function DownloadOverlay({ href, name, label }: { href: string; name: string; label: string }) {
  return (
    <a
      href={href}
      download={name}
      aria-label={label}
      title={label}
      className="bg-background/75 text-foreground hover:bg-background absolute top-2 right-2 grid size-8 place-items-center rounded-lg opacity-0 backdrop-blur-sm transition-opacity group-hover:opacity-100 focus-visible:opacity-100 max-lg:opacity-100"
    >
      <Download className="size-4" />
    </a>
  )
}

/** A non-media attachment: a compact document chip that downloads the authed
 *  bytes on click (no preview to preload — documents fetch lazily). */
function NeoFileChip({ url, name }: { url: string; name?: string }) {
  const t = useTranslations('agentChat')
  const label = name || mediaFileName(url, 'file')
  const [busy, setBusy] = useState(false)
  return (
    <button
      type="button"
      title={t('mediaDownload')}
      disabled={busy}
      onClick={async () => {
        setBusy(true)
        const ok = await downloadMediaRef(url, label)
        setBusy(false)
        if (!ok) toastUnavailable(t('mediaUnavailable'))
      }}
      className="bg-background/60 hover:bg-background text-foreground flex max-w-xs items-center gap-2 rounded-xl px-3 py-2 text-left text-sm transition-colors disabled:cursor-wait disabled:opacity-70"
    >
      <FileText className="text-muted-foreground size-4 shrink-0" />
      <span className="min-w-0 truncate">{label}</span>
      <Download className="text-muted-foreground ml-auto size-3.5 shrink-0" />
    </button>
  )
}

// Chip failures surface inline (the chip itself stays); a console note is
// enough — the user just sees the download not start and can retry.
function toastUnavailable(msg: string) {
  console.warn(msg)
}

export function useAuthedMediaURL(
  ref?: string,
  disabled = false,
): {
  src: string | null
  failed: boolean
} {
  const [src, setSrc] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    let obj: string | null = null
    setSrc(null)
    setFailed(false)
    if (!ref || disabled) return
    loadMediaObjectURL(ref).then((loaded) => {
      if (!active) {
        if (loaded) URL.revokeObjectURL(loaded)
        return
      }
      if (loaded) {
        obj = loaded
        setSrc(loaded)
      } else {
        setFailed(true)
      }
    })
    return () => {
      active = false
      if (obj) URL.revokeObjectURL(obj)
    }
  }, [disabled, ref])

  return { src, failed }
}

/* ------------------------------------------------------------------ */
/* Post-generation prompt intelligence                                 */
/* ------------------------------------------------------------------ */

/** Common directional modifiers offered after a generation. */
const BASE_DIRECTIONS = [
  'more cinematic lighting and deeper contrast',
  'softer natural light and gentler color grade',
  'tighter close-up on the main subject',
  'wider establishing shot with more environment',
  'richer detail and sharper textures',
  'simpler composition with a cleaner background',
  'warmer golden-hour palette',
  'cooler blue-hour / night mood',
] as const

/**
 * Smart prompt recommendations shown under a generated image.
 * Pure, deterministic: keyed off the original prompt so chips stay stable
 * across re-renders. Never invents a second model call — heuristics only.
 */
export function suggestImagePrompts(prompt?: string, limit = 5): string[] {
  const p = (prompt || '').trim()
  const lower = p.toLowerCase()
  const out: string[] = []
  const push = (s: string) => {
    const t = s.trim()
    if (!t) return
    if (out.some((x) => x.toLowerCase() === t.toLowerCase())) return
    out.push(t)
  }

  // Prompt-aware pivots (subject / style / mood).
  if (/\b(portrait|face|person|man|woman|people)\b/.test(lower)) {
    push('same subject, three-quarter portrait, shallow depth of field')
    push('same subject, full-body environmental portrait')
  }
  if (/\b(logo|icon|mark|brand)\b/.test(lower)) {
    push('flat vector logo on a clean solid background')
    push('the same mark as a subtle embossed metallic badge')
  }
  if (/\b(product|packaging|bottle|shoe|device)\b/.test(lower)) {
    push('studio product shot on seamless background, soft rim light')
    push('lifestyle product placement in a real scene')
  }
  if (/\b(landscape|city|skyline|mountain|forest|ocean|beach)\b/.test(lower)) {
    push('same scene at golden hour with long shadows')
    push('same scene in foggy early morning light')
  }
  if (/\b(cyber|neon|futur|sci-?fi|matrix)\b/.test(lower)) {
    push('more neon rain reflections and volumetric light shafts')
    push('desaturated noir cyberpunk, single accent color')
  }
  if (/\b(cartoon|anime|illustration|comic|sketch)\b/.test(lower)) {
    push('same composition as a clean flat illustration')
    push('same subject as a detailed painterly concept art piece')
  }
  if (/\b(photo|realistic|photoreal)\b/.test(lower) || !p) {
    push('ultra-photoreal, natural lens imperfections, subtle grain')
  }

  // Always offer a few base directions not already implied by the prompt.
  for (const d of BASE_DIRECTIONS) {
    if (lower && lower.includes(d.split(' ')[0]!)) continue
    push(d)
    if (out.length >= limit) break
  }

  // If we still have room and a prompt exists, offer a "keep subject, new style" pack.
  if (p && out.length < limit) {
    push(`keep the subject of “${truncate(p, 48)}”, restyle as film still`)
  }

  return out.slice(0, limit)
}

function truncate(s: string, n: number): string {
  const t = s.trim()
  return t.length <= n ? t : `${t.slice(0, n - 1)}…`
}

/** Build the prose Neo receives for a targeted edit (tweak). */
export function composeImageTweak(url: string, change: string): string {
  const body = change.trim()
  return [
    `Edit the image at ${url}.`,
    `Here is exactly what I want changed (treat this as the brief — highlight these differences and apply them):`,
    body,
    `Use edit_image on that media reference. Keep everything I did not mention the same.`,
  ].join('\n')
}

/** Build the prose Neo receives for variation generation. */
export function composeImageVariations(url: string, prompt?: string, count = 3): string {
  const n = Math.max(2, Math.min(count, 4))
  const base = prompt?.trim()
    ? `Original prompt was: “${prompt.trim()}”.`
    : 'Use the attached image as the composition and subject reference.'
  return [
    `Generate ${n} variations of the image at ${url}.`,
    base,
    `Keep the same subject and overall composition; explore different lighting, color grade, and small detail changes.`,
    `Return each variation as its own image.`,
  ].join('\n')
}

/** Build the prose Neo receives when a recommendation chip is picked. */
export function composeImageSuggestion(url: string, suggestion: string, prompt?: string): string {
  const dir = suggestion.trim()
  if (prompt?.trim()) {
    return [
      `Create an image: ${prompt.trim()}, ${dir}.`,
      `You may reference the previous result at ${url} for composition and subject fidelity.`,
    ].join('\n')
  }
  return [
    `Create an image: ${dir}.`,
    `Use the previous result at ${url} as a subject/composition reference when helpful.`,
  ].join('\n')
}

/* ------------------------------------------------------------------ */
/* Post-generation control surface (images)                            */
/* ------------------------------------------------------------------ */

function ImagePostSurface({
  url,
  prompt,
  onAction,
}: {
  url: string
  prompt?: string
  onAction: MediaActionHandler
}) {
  const [tweaking, setTweaking] = useState(false)
  const [change, setChange] = useState('')
  const suggestions = useMemo(() => suggestImagePrompts(prompt), [prompt])

  const submitTweak = () => {
    const body = change.trim()
    if (!body) return
    onAction(composeImageTweak(url, body))
    setChange('')
    setTweaking(false)
  }

  return (
    <div className="bg-muted/40 mt-2 flex w-full max-w-md flex-col gap-2 rounded-xl px-3 py-2.5">
      {/* primary actions — tone-only separation, no border strokes */}
      <div className="flex flex-wrap items-center gap-1.5">
        <button
          type="button"
          onClick={() => setTweaking((v) => !v)}
          aria-expanded={tweaking}
          className={cn(
            'flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[0.75rem] font-medium transition-colors',
            tweaking
              ? 'bg-primary text-primary-foreground'
              : 'bg-background/70 text-foreground hover:bg-background',
          )}
        >
          <SlidersHorizontal className="size-3.5 opacity-80" />
          Tweak
        </button>
        <button
          type="button"
          onClick={() => onAction(composeImageVariations(url, prompt))}
          className="bg-background/70 text-foreground hover:bg-background flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[0.75rem] font-medium transition-colors"
        >
          <RotateCcw className="size-3.5 opacity-80" />
          Variations
        </button>
      </div>

      {/* tweak panel — explain what to change; Neo gets the media ref + brief */}
      {tweaking && (
        <div className="flex flex-col gap-2">
          <label className="text-muted-foreground text-[0.7rem] leading-snug">
            Highlight what to change — Neo will edit this image from your brief.
          </label>
          <textarea
            value={change}
            onChange={(e) => setChange(e.target.value)}
            rows={3}
            placeholder="e.g. make the sky stormier, remove the text in the corner, warmer skin tones…"
            className="bg-background text-foreground placeholder:text-muted-foreground/60 w-full resize-none rounded-lg px-2.5 py-2 text-sm leading-relaxed outline-none"
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault()
                submitTweak()
              }
              if (e.key === 'Escape') {
                setTweaking(false)
                setChange('')
              }
            }}
          />
          <div className="flex items-center justify-end gap-1.5">
            <button
              type="button"
              onClick={() => {
                setTweaking(false)
                setChange('')
              }}
              className="text-muted-foreground hover:bg-background hover:text-foreground grid size-8 place-items-center rounded-full transition-colors"
              aria-label="Cancel tweak"
            >
              <X className="size-3.5" />
            </button>
            <button
              type="button"
              onClick={submitTweak}
              disabled={!change.trim()}
              className="bg-primary text-primary-foreground hover:bg-primary/90 flex items-center gap-1.5 rounded-full px-3 py-1.5 text-[0.75rem] font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40"
            >
              <Send className="size-3.5" />
              Send to Neo
            </button>
          </div>
        </div>
      )}

      {/* smart prompt recommendations */}
      {suggestions.length > 0 && !tweaking && (
        <div className="flex flex-col gap-1.5">
          <p className="text-muted-foreground flex items-center gap-1.5 text-[0.68rem] font-medium tracking-wide uppercase">
            <Lightbulb className="size-3 opacity-70" />
            Try next
          </p>
          <div className="flex flex-wrap gap-1.5">
            {suggestions.map((s) => (
              <button
                key={s}
                type="button"
                title={s}
                onClick={() => onAction(composeImageSuggestion(url, s, prompt))}
                className="bg-background/70 text-muted-foreground hover:bg-background hover:text-foreground max-w-full truncate rounded-full px-2.5 py-1 text-left text-[0.72rem] transition-colors"
              >
                {s}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

/** Render a single `/media/<name>` reference behind per-user auth. */
export function NeoMediaItem({
  url,
  kind,
  prompt,
  name,
  onAction,
}: ParsedMedia & { onAction?: MediaActionHandler }) {
  const t = useTranslations('agentChat')
  const isFile = kind === 'file'
  const { src, failed } = useAuthedMediaURL(url, isFile)

  if (isFile) {
    return <NeoFileChip url={url} name={name} />
  }
  if (failed) {
    return <span className="text-muted-foreground text-xs">{t('mediaUnavailable')}</span>
  }
  if (!src) {
    return <div className="bg-muted h-44 w-full max-w-sm animate-pulse rounded-xl" aria-hidden />
  }
  if (kind === 'video') {
    return (
      <div className="group relative w-full max-w-md">
        <video src={src} controls className="max-h-[26rem] w-full rounded-xl" preload="metadata" />
        <DownloadOverlay href={src} name={mediaFileName(url, kind)} label={t('mediaDownload')} />
      </div>
    )
  }
  if (kind === 'audio') {
    return (
      <AudioPlayer className="bg-card text-foreground w-full max-w-md rounded-xl px-2 py-1">
        <AudioPlayerElement src={src} preload="metadata" />
        <AudioPlayerControlBar className="flex w-full items-center gap-1 bg-transparent">
          <AudioPlayerPlayButton />
          <AudioPlayerTimeDisplay />
          <AudioPlayerTimeRange className="min-w-20 flex-1" />
          <AudioPlayerDurationDisplay />
          <AudioPlayerMuteButton />
          <AudioPlayerVolumeRange className="w-16" />
        </AudioPlayerControlBar>
      </AudioPlayer>
    )
  }
  return (
    <div className="flex w-full max-w-md flex-col">
      <div className="group relative w-full">
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img
          src={src}
          alt={prompt || t('mediaImageAlt')}
          className="max-h-[26rem] w-full rounded-xl object-contain"
        />
        <DownloadOverlay href={src} name={mediaFileName(url, kind)} label={t('mediaDownload')} />
      </div>
      {onAction ? <ImagePostSurface url={url} prompt={prompt} onAction={onAction} /> : null}
    </div>
  )
}

/** ChatGPT-style generation placeholder: the reserved frame where the media
 *  will appear, a dot lattice breathing under it while the model renders.
 *  Tone-only (bg-card), no borders; stills under prefers-reduced-motion. */
export function NeoMediaSkeleton({ label }: { label?: string }) {
  const t = useTranslations('agentChat')
  const text = label || t('mediaGenerating')
  return (
    <div
      role="status"
      aria-label={text}
      className="bg-card relative aspect-square w-full max-w-sm overflow-hidden rounded-2xl"
    >
      <div
        aria-hidden
        className="absolute inset-0 animate-pulse motion-reduce:animate-none"
        style={{
          backgroundImage:
            'radial-gradient(color-mix(in oklab, var(--foreground) 20%, transparent) 1.5px, transparent 1.5px)',
          backgroundSize: '16px 16px',
          maskImage: 'radial-gradient(ellipse 70% 70% at 50% 55%, black, transparent 75%)',
          WebkitMaskImage: 'radial-gradient(ellipse 70% 70% at 50% 55%, black, transparent 75%)',
        }}
      />
      <span className="text-muted-foreground/80 absolute bottom-3 left-1/2 -translate-x-1/2 text-xs">
        <span className="shimmer">{text}</span>
      </span>
    </div>
  )
}

/** Render a list of generated media artifacts (from a tool.media event). */
export function NeoMediaGrid({
  media,
  onAction,
}: {
  media?: ChatMedia[]
  /** When set, each generated image shows the post-gen tweak / variations /
   *  recommendation surface and routes actions back to Neo. */
  onAction?: MediaActionHandler
}) {
  if (!media || media.length === 0) return null
  return (
    <div className="mt-2 flex flex-col gap-2">
      {media.map((mi, i) => (
        <NeoMediaItem
          key={`${mi.url}-${i}`}
          url={mi.url}
          kind={mi.kind}
          prompt={mi.prompt}
          onAction={mi.kind === 'image' ? onAction : undefined}
        />
      ))}
    </div>
  )
}
