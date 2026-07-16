'use client'

/**
 * neo-message — the chat-turn presentation for the Adaptive Surface.
 *
 * Ports the look & feel of `components/thread.tsx` (avatar, per-message action
 * bar, reasoning channel, inline error, thinking indicator) onto Neo's own
 * `useChat` data model — no @assistant-ui runtime. Separation is by TONE only
 * (no borders for depth, per the design system); the single accent is #004ced.
 *
 * Chat avatars: per the product call, both the user and Neo render one of the
 * five bundled identicon glyphs (`/public/1.svg … /5.svg`), picked
 * deterministically per turn so it reads as "random" yet never flickers across
 * re-renders.
 */
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Check, ChevronDown, Copy, Download, BrainIcon, Music2Icon } from '@/lib/matrix-icons'
import { WaveSpinner } from '@/components/ui/wave-spinner'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { MessageResponse } from '@/components/ai-elements/message'
import { MarkdownErrorBoundary } from '@/components/ai-elements/markdown-error-boundary'
import { TooltipIconButton } from '@/components/assistant-ui/tooltip-icon-button'
import { NeoMediaGrid, NeoMediaItem, parseAttachments } from '@/components/matrix/neo/neo-media'
import { PixelGrid, WaveBars } from '@/components/matrix/cody/loaders'
import { cn } from '@/lib/utils'
import type { ChatMessage } from '@/hooks/api/useChat'

/** Number of bundled identicon avatars in /public (1.svg … N.svg). */
const AVATAR_COUNT = 5

/** Map an arbitrary seed → a stable avatar index in [1, AVATAR_COUNT]. */
function avatarIndex(seed: string): number {
  let h = 0
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) | 0
  return (Math.abs(h) % AVATAR_COUNT) + 1
}

/** A chat avatar: one of the bundled identicon glyphs, chosen per `seed`.
 *  Rendered as a background image so we don't trip the next/image lint and
 *  keep it purely decorative (aria-hidden). */
export function ChatAvatar({ seed, className }: { seed: string; className?: string }) {
  const n = avatarIndex(seed)
  return (
    <span
      aria-hidden
      className={cn(
        'bg-card block size-8 shrink-0 rounded-full bg-cover bg-center bg-no-repeat',
        className,
      )}
      style={{ backgroundImage: `url(/${n}.svg)` }}
    />
  )
}

/** Shared assistant-prose styling: keeps long unbroken strings (hashes, URLs)
 *  inside the column; code blocks / tables scroll internally. */
export const NEO_PROSE_CLASS = cn(
  'text-foreground min-w-0 text-[0.95rem] leading-relaxed [overflow-wrap:anywhere]',
  '[&_a]:text-primary [&_a]:break-words [&_a]:underline [&_a]:underline-offset-2',
  '[&_pre]:max-w-full [&_pre]:overflow-x-auto [&_code]:break-words',
  '[&_img]:max-w-full [&_img]:rounded-xl',
  '[&_table]:block [&_table]:max-w-full [&_table]:overflow-x-auto',
)

/** Render markdown prose with a plain-text fallback if rendering throws.
 *  While `streaming`, Streamdown runs in streaming mode (incomplete-markdown
 *  safe) and — unless reduced motion — fades new tokens in, reusing its
 *  prev-content-length tracking so already-visible prose is never re-animated.
 *  That plus the rAF-coalesced delta feed is what keeps the live answer smooth
 *  instead of repainting the whole block per token. */
function Prose({
  text,
  streaming,
  animate,
}: {
  text: string
  streaming?: boolean
  animate?: boolean
}) {
  return (
    <MarkdownErrorBoundary
      resetKey={text}
      fallback={
        <pre className="text-foreground max-w-full overflow-x-auto text-[0.9rem] whitespace-pre-wrap">
          {text}
        </pre>
      }
    >
      <MessageResponse
        {...(streaming
          ? { mode: 'streaming' as const, isAnimating: true, animated: animate ?? true }
          : {})}
      >
        {text}
      </MessageResponse>
    </MarkdownErrorBoundary>
  )
}

/** The model's chain-of-thought, surfaced as a collapsible channel — never
 *  part of the answer. Tone-only (bg-muted), no border. */
export function NeoReasoning({ reasoning }: { reasoning: string }) {
  const [open, setOpen] = useState(false)
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="bg-muted/40 mb-3 w-full rounded-xl">
      <CollapsibleTrigger className="text-muted-foreground hover:text-foreground group/reason flex w-full items-center gap-2 px-3 py-2 text-sm transition-colors">
        <BrainIcon className="size-4 shrink-0" />
        <span>Reasoning</span>
        <ChevronDown
          className={cn(
            'ml-auto size-4 shrink-0 transition-transform',
            open ? 'rotate-0' : '-rotate-90',
          )}
        />
      </CollapsibleTrigger>
      <CollapsibleContent className="text-muted-foreground overflow-hidden text-sm">
        <div className="max-h-72 overflow-y-auto px-3 pt-1 pb-3 leading-relaxed">
          <Prose text={reasoning} />
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

/** Per-message actions: copy, export as Markdown, read aloud. */
function NeoMessageActions({ text }: { text: string }) {
  const [copied, setCopied] = useState(false)
  const [speaking, setSpeaking] = useState(false)
  const copyTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const onCopy = useCallback(() => {
    void navigator.clipboard?.writeText(text).then(() => {
      setCopied(true)
      if (copyTimer.current) clearTimeout(copyTimer.current)
      copyTimer.current = setTimeout(() => setCopied(false), 1500)
    })
  }, [text])

  const onExport = useCallback(() => {
    const blob = new Blob([text], { type: 'text/markdown' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'neo-message.md'
    a.click()
    URL.revokeObjectURL(url)
  }, [text])

  const onSpeak = useCallback(() => {
    const synth = typeof window !== 'undefined' ? window.speechSynthesis : null
    if (!synth) return
    if (synth.speaking) {
      synth.cancel()
      setSpeaking(false)
      return
    }
    const utter = new SpeechSynthesisUtterance(text)
    utter.onend = () => setSpeaking(false)
    utter.onerror = () => setSpeaking(false)
    setSpeaking(true)
    synth.speak(utter)
  }, [text])

  // Stop any in-flight narration if the message unmounts.
  useEffect(
    () => () => {
      if (copyTimer.current) clearTimeout(copyTimer.current)
      if (typeof window !== 'undefined' && window.speechSynthesis?.speaking) {
        window.speechSynthesis.cancel()
      }
    },
    [],
  )

  return (
    <div className="text-muted-foreground -ml-1 flex items-center gap-0.5">
      <TooltipIconButton tooltip={copied ? 'Copied' : 'Copy'} onClick={onCopy}>
        {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
      </TooltipIconButton>
      <TooltipIconButton tooltip="Export as Markdown" onClick={onExport}>
        <Download className="size-4" />
      </TooltipIconButton>
      <TooltipIconButton tooltip={speaking ? 'Stop' : 'Read aloud'} onClick={onSpeak}>
        <Music2Icon className={cn('size-4', speaking && 'text-primary')} />
      </TooltipIconButton>
    </div>
  )
}

/** A settled assistant turn: prose (+ reasoning, media, actions), no avatar
 *  — the identicon is reserved for Neo's live-status slot only. When
 *  `failed` the prose renders in a tone-only error block. */
export function NeoAssistantMessage({
  message,
  failed,
  onMediaAction,
}: {
  message: ChatMessage
  failed?: boolean
  /** Post-generation image actions (tweak / variations / suggestions) → Neo. */
  onMediaAction?: (instruction: string) => void
}) {
  return (
    <div className="flex w-full min-w-0 flex-col gap-1">
      {message.reasoning && <NeoReasoning reasoning={message.reasoning} />}
      {failed ? (
        <div className="bg-destructive/10 text-destructive rounded-lg px-3 py-2 text-sm">
          <Prose text={message.text} />
        </div>
      ) : (
        <div className={NEO_PROSE_CLASS}>
          <Prose text={message.text} />
        </div>
      )}
      <NeoMediaGrid media={message.media} onAction={onMediaAction} />
      {message.text && <NeoMessageActions text={message.text} />}
    </div>
  )
}

/** A user turn: a right-aligned accent bubble, no avatar. Uploaded
 *  attachments render as thumbnails; their raw markers are stripped from text. */
export function NeoUserMessage({ message }: { message: ChatMessage }) {
  const { clean, items } = parseAttachments(message.text)
  return (
    <div className="flex w-full justify-end">
      <div className="bg-accent text-foreground max-w-[85%] min-w-0 rounded-2xl rounded-br-md px-4 py-2.5 text-[0.925rem] leading-relaxed [overflow-wrap:anywhere]">
        {items.length > 0 && (
          <div className="mb-2 flex flex-col gap-2">
            {items.map((it, i) => (
              <NeoMediaItem key={`${it.url}-${i}`} url={it.url} kind={it.kind} name={it.name} />
            ))}
          </div>
        )}
        {clean && <span className="whitespace-pre-wrap">{clean}</span>}
      </div>
    </div>
  )
}

/** The live "thinking / working" indicator, shown as a nascent assistant turn
 *  (mark + shimmering label) while the run has not yet produced prose. The
 *  mark is Neo's live-status slot: Pixel Grid while idle/not yet responding. */
export function NeoThinking({ label, reduce }: { label: string; reduce: boolean }) {
  return (
    <div className="flex w-full gap-3">
      <span className="grid size-8 shrink-0 place-items-center">
        <PixelGrid size={24} />
      </span>
      <div className="text-muted-foreground flex items-center gap-2 pt-1.5 text-sm">
        <WaveSpinner
          size="sm"
          color="var(--primary)"
          duration={reduce ? 0 : 0.7}
          aria-label={label}
        />
        <span className="shimmer">{label}</span>
      </div>
    </div>
  )
}

/** Live chain-of-thought, streamed token-by-token while Neo works. Unlike the
 *  collapsible post-hoc NeoReasoning, this is shown open + auto-scrolling so the
 *  user can watch the reasoning unfold. Never the answer; clears with the turn. */
function NeoLiveThinking({ text }: { text: string }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = ref.current
    if (el) el.scrollTop = el.scrollHeight
  }, [text])
  return (
    <div className="bg-muted/40 mb-1 w-full overflow-hidden rounded-xl">
      <div className="text-muted-foreground flex items-center gap-2 px-3 pt-2 text-xs font-medium">
        <BrainIcon className="size-3.5 shrink-0" />
        <span className="shimmer">Thinking</span>
      </div>
      <div
        ref={ref}
        className="text-muted-foreground/80 max-h-36 overflow-y-auto px-3 pt-1 pb-2.5 text-xs leading-relaxed whitespace-pre-wrap"
      >
        {text}
      </div>
    </div>
  )
}

/** A thin blinking caret that marks the answer as still being typed. */
function StreamCaret({ reduce }: { reduce: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        'bg-primary ml-0.5 inline-block h-[1.05em] w-[2px] translate-y-[2px] rounded-[1px] align-middle',
        !reduce && 'animate-pulse',
      )}
    />
  )
}

/** The live, in-flight assistant turn rendered in the conversation rail: the
 *  streaming chain-of-thought (when surfaced) and the answer being typed out
 *  token-by-token. Before any prose arrives it falls back to the shimmering
 *  "thinking / working" indicator so the surface is never blank during the wait.
 *  `thinking` is omitted by the caller when another pane already shows it. */
export function NeoLiveTurn({
  thinking,
  streamingAnswer,
  label,
  reduce,
  seed = 'neo-thinking',
}: {
  thinking?: string
  streamingAnswer?: string
  label: string
  reduce: boolean
  seed?: string
}) {
  const answer = streamingAnswer?.trim() ? streamingAnswer : ''
  const thoughts = thinking?.trim() ? thinking : ''
  // Nothing to show yet → the nascent shimmer indicator (mark + label).
  if (!answer && !thoughts) {
    return <NeoThinking label={label} reduce={reduce} />
  }
  return (
    <NeoAssistantRow
      seed={seed}
      avatar={
        <span className="grid size-8 shrink-0 place-items-center">
          <WaveBars size={24} bars={4} />
        </span>
      }
    >
      {thoughts && <NeoLiveThinking text={thoughts} />}
      {answer ? (
        <div className={NEO_PROSE_CLASS}>
          <Prose text={answer} streaming animate={!reduce} />
          <StreamCaret reduce={reduce} />
        </div>
      ) : (
        <div className="text-muted-foreground flex items-center gap-2 pt-0.5 text-sm">
          <WaveSpinner
            size="sm"
            color="var(--primary)"
            duration={reduce ? 0 : 0.7}
            aria-label={label}
          />
          <span className="shimmer">{label}</span>
        </div>
      )}
    </NeoAssistantRow>
  )
}

/** Generic slot wrapper used where the surface needs an assistant-styled row
 *  around arbitrary content (e.g. the settled task answer). `avatar` overrides
 *  the default identicon — callers on Neo's live-status slot pass a spinner. */
export function NeoAssistantRow({
  seed,
  avatar,
  children,
}: {
  seed: string
  avatar?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="flex w-full gap-3">
      {avatar ?? <ChatAvatar seed={seed} />}
      <div className="flex min-w-0 flex-1 flex-col gap-1">{children}</div>
    </div>
  )
}
