'use client'

/**
 * NeoComposer — the single INTENT primitive of the Adaptive Surface.
 *
 * One controlled composer reused in two places: the idle PILL and the open
 * task CARD's footer. Because it lives in the card too, the user can always
 * reply / interrupt / redirect — the surface never traps them on a finished
 * answer with no way to respond.
 *
 * Layout follows the Perplexity composer pattern: the auto-growing textarea
 * (`text-base`, `leading-6`) sits ON TOP and a fixed toolbar of round 36px
 * controls sits BELOW it, so a tall multi-line entry grows the box DOWNWARD
 * while every control stays pinned and all text stays in view. Both variants
 * share this stacked shape; they differ only in their container — the `bar`
 * variant brings its own rounded ring panel for the docked footer, while the
 * `pill` variant fills the morphing surface, which paints the rounded
 * background + ring itself.
 *
 * The left control is a TOOL SELECTOR (replacing the old brand mark): the user
 * picks a focused mode (web / price / code / wallet) BEFORE sending, so the
 * agent knows which capability to reach for. Mode `auto` lets Neo decide.
 * Selection only shapes the outgoing prose (see `compose`) — no protocol
 * jargon leaks to the user.
 */
import { useCallback, useEffect, useRef, type RefObject } from 'react'
import {
  ArrowUp,
  Check,
  Code,
  Coins,
  Gauge,
  Globe,
  ImageIcon,
  Lightbulb,
  Music2Icon,
  Paperclip,
  Plus,
  SquareIcon,
  VideoIcon,
  Wallet,
  X,
} from '@/lib/matrix-icons'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { cn } from '@/lib/utils'

export type NeoMode = 'auto' | 'plan' | 'web' | 'price' | 'code' | 'wallet' | 'image' | 'video'

type IconType = typeof Globe

interface ModeDef {
  id: NeoMode
  label: string
  /** One-line description shown in the menu. */
  hint: string
  icon: IconType
  /** Shape the outgoing message from the user's typed argument. An empty
   *  argument falls back to a sensible canned action (the old button behaviour,
   *  but now optional rather than the only option). */
  compose: (text: string) => string
}

export const NEO_MODES: ModeDef[] = [
  {
    id: 'auto',
    label: 'Auto',
    hint: 'Let Neo pick the right tools',
    icon: Gauge,
    compose: (t) => t,
  },
  {
    id: 'plan',
    label: 'Plan first',
    hint: 'Explore and propose a plan before making any changes',
    icon: Lightbulb,
    compose: (t) =>
      t
        ? `Plan mode: explore and think this through, then propose a clear step-by-step plan for the following and ask me any focused clarifying questions you need. Do NOT make any changes, send anything, or take any irreversible or value-moving action yet — wait for my go-ahead first. Task: ${t}`
        : 'Plan mode: ask me what I want to accomplish, then propose a step-by-step plan before making any changes — wait for my go-ahead before acting.',
  },
  {
    id: 'web',
    label: 'Search the web',
    hint: 'Answer from live web results',
    icon: Globe,
    compose: (t) =>
      t ? `Search the web and answer: ${t}` : 'Search the web for the latest on Paxeer',
  },
  {
    id: 'price',
    label: 'Check PAX price',
    hint: 'Live PAX price and 24h move',
    icon: Coins,
    compose: (t) =>
      t ? `Check the PAX price, then ${t}` : 'What is the current PAX price and 24h change?',
  },
  {
    id: 'code',
    label: 'Write code',
    hint: 'Draft or edit code',
    icon: Code,
    compose: (t) => (t ? `Write code: ${t}` : 'Write a TypeScript function to debounce a callback'),
  },
  {
    id: 'image',
    label: 'Create an image',
    hint: 'Generate a new image — or edit an attached one',
    icon: ImageIcon,
    compose: (t) =>
      t ? `Create an image: ${t}` : 'I want to create an image — ask me what I have in mind',
  },
  {
    id: 'video',
    label: 'Create a video',
    hint: 'Generate a short video — or animate an attached image',
    icon: VideoIcon,
    compose: (t) =>
      t ? `Create a video: ${t}` : 'I want to create a short video — ask me what I have in mind',
  },
  {
    id: 'wallet',
    label: 'Send PAX',
    hint: 'Transfer PAX — needs your approval',
    icon: Wallet,
    compose: (t) => (t ? `Send PAX: ${t}` : 'I want to send PAX — walk me through it'),
  },
]

export function neoModeDef(id: NeoMode): ModeDef {
  return NEO_MODES.find((m) => m.id === id) ?? NEO_MODES[0]
}

/** Turn the current mode + raw input into the prose actually sent to Neo. */
export function composeNeoMessage(mode: NeoMode, raw: string): string {
  return neoModeDef(mode).compose(raw.trim()).trim()
}

/** Cap the auto-grow height of the textarea (px). Mirrors Grok's `max-h-*`. */
const MAX_TEXTAREA_HEIGHT = 160

/** Bucket a File's MIME into the chip icon. */
function AttachmentIcon({ mime, className }: { mime: string; className?: string }) {
  if (mime.startsWith('image/')) return <ImageIcon className={className} />
  if (mime.startsWith('video/')) return <VideoIcon className={className} />
  if (mime.startsWith('audio/')) return <Music2Icon className={className} />
  return <Paperclip className={className} />
}

export function NeoComposer({
  value,
  onChange,
  onSubmit,
  mode,
  onModeChange,
  variant = 'pill',
  inputRef,
  placeholder = 'Ask Neo anything…',
  attachments = [],
  onAddFiles,
  onRemoveFile,
  uploading = false,
  isRunning = false,
  onStop,
}: {
  value: string
  onChange: (v: string) => void
  onSubmit: () => void
  mode: NeoMode
  onModeChange: (m: NeoMode) => void
  variant?: 'pill' | 'bar'
  inputRef?: RefObject<HTMLTextAreaElement | null>
  placeholder?: string
  /** Files staged for upload, rendered as removable chips above the input. */
  attachments?: File[]
  /** Add picked files to the staging tray (enables the attach button). */
  onAddFiles?: (files: File[]) => void
  /** Remove one staged file by index. */
  onRemoveFile?: (index: number) => void
  /** True while staged files are being uploaded — locks the composer. */
  uploading?: boolean
  /** True while a run is live — the primary control becomes a STOP button
   *  (mirrors thread.tsx's send⇄cancel toggle). Typing + Enter still sends a
   *  redirect, preserving Neo's mid-run interrupt. */
  isRunning?: boolean
  /** Abort the live run. Required for the stop toggle to appear. */
  onStop?: () => void
}) {
  const pill = variant === 'pill'
  const active = neoModeDef(mode)
  const ActiveIcon = active.icon

  const internalRef = useRef<HTMLTextAreaElement>(null)
  const taRef = inputRef ?? internalRef
  const fileRef = useRef<HTMLInputElement>(null)

  // Auto-grow: reset to single-row, then expand to content up to the cap so the
  // composer behaves like Grok's multi-line input across every screen size.
  useEffect(() => {
    const el = taRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${Math.min(el.scrollHeight, MAX_TEXTAREA_HEIGHT)}px`
  }, [value, taRef])

  const pickMode = useCallback(
    (m: NeoMode) => {
      onModeChange(m)
      // Keep the composer hot so the user can type the argument right away.
      window.setTimeout(() => taRef.current?.focus(), 0)
    },
    [onModeChange, taRef],
  )

  // Tool selector — greyish, neutral chrome (the active mode reads from its
  // icon, so it no longer needs the accent fill).
  const toolSelector = (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          aria-label={`Tool: ${active.label}`}
          title={`Tool: ${active.label}`}
          className="group relative flex size-9 shrink-0 items-center justify-center rounded-full bg-zinc-200 text-zinc-500 transition-colors hover:bg-zinc-300 data-[state=open]:bg-zinc-300 data-[state=open]:text-zinc-700 dark:bg-zinc-700 dark:text-zinc-400 dark:hover:bg-zinc-600/90 dark:data-[state=open]:bg-zinc-600 dark:data-[state=open]:text-zinc-100"
        >
          <ActiveIcon className="size-[18px] shrink-0" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="start"
        side={pill ? 'bottom' : 'top'}
        sideOffset={10}
        className="w-64"
      >
        {NEO_MODES.map((m) => {
          const Icon = m.icon
          const selected = m.id === mode
          return (
            <DropdownMenuItem key={m.id} onSelect={() => pickMode(m.id)} className="gap-2.5 py-2">
              <span
                className={cn(
                  'grid size-7 shrink-0 place-items-center rounded-lg',
                  selected ? 'bg-foreground text-background' : 'bg-muted text-muted-foreground',
                )}
              >
                <Icon className="size-4" />
              </span>
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="text-foreground text-sm font-semibold">{m.label}</span>
                <span className="text-muted-foreground truncate text-xs">{m.hint}</span>
              </span>
              {selected && <Check className="text-foreground size-4 shrink-0" />}
            </DropdownMenuItem>
          )
        })}
      </DropdownMenuContent>
    </DropdownMenu>
  )

  // Attach — stage image / video / audio for upload to the agent volume.
  const attachButton = onAddFiles && (
    <>
      <input
        ref={fileRef}
        type="file"
        accept="image/*,video/*,audio/*"
        multiple
        className="hidden"
        onChange={(e) => {
          const picked = Array.from(e.target.files ?? [])
          if (picked.length) onAddFiles(picked)
          e.target.value = ''
        }}
      />
      <button
        type="button"
        aria-label="Attach a file"
        title="Attach an image, video, or audio file"
        onClick={() => fileRef.current?.click()}
        disabled={uploading}
        className="group relative flex size-9 shrink-0 items-center justify-center rounded-full bg-zinc-200 text-zinc-500 transition-colors hover:bg-zinc-300 disabled:cursor-wait disabled:opacity-70 dark:bg-zinc-700 dark:text-zinc-400 dark:hover:bg-zinc-600/90"
      >
        <Plus className="size-[22px]" />
      </button>
    </>
  )

  const textareaEl = (
    <textarea
      ref={taRef}
      rows={1}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === 'Enter' && !e.shiftKey) {
          e.preventDefault()
          onSubmit()
        }
      }}
      placeholder={uploading ? 'Uploading…' : placeholder}
      autoComplete="off"
      aria-label="Ask Neo"
      disabled={uploading}
      className="neo-input text-foreground placeholder:text-muted-foreground/70 min-h-9 w-full min-w-0 flex-1 resize-none bg-transparent py-1.5 text-base leading-6 font-medium outline-none disabled:opacity-60"
    />
  )

  // Primary control — greyish circle; toggles to STOP while a run is live.
  const primaryButton =
    isRunning && onStop ? (
      <button
        type="button"
        onClick={onStop}
        aria-label="Stop"
        title="Stop"
        className="flex size-9 min-w-9 shrink-0 items-center justify-center rounded-full bg-zinc-200 text-zinc-600 transition-colors hover:bg-zinc-300 dark:bg-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-600"
      >
        <SquareIcon className="size-3.5 fill-current" />
      </button>
    ) : (
      <button
        type="button"
        onClick={onSubmit}
        aria-label="Send"
        disabled={uploading}
        className="bg-foreground text-background hover:bg-foreground/90 flex size-9 min-w-9 shrink-0 items-center justify-center rounded-full transition-colors disabled:cursor-not-allowed disabled:bg-zinc-200 disabled:text-zinc-400 dark:disabled:bg-zinc-700 dark:disabled:text-zinc-500"
      >
        <ArrowUp className="size-[18px]" />
      </button>
    )

  // Removable chips for the staged uploads, rendered above the input.
  const chips = attachments.length > 0 && (
    <div className="flex flex-wrap gap-2 px-1 pb-1">
      {attachments.map((f, i) => (
        <span
          key={`${f.name}-${i}`}
          className="bg-accent text-foreground flex items-center gap-1.5 rounded-lg py-1 pr-1 pl-2 text-xs"
        >
          <AttachmentIcon mime={f.type} className="text-muted-foreground size-3.5 shrink-0" />
          <span className="max-w-[10rem] truncate">{f.name}</span>
          {onRemoveFile && (
            <button
              type="button"
              aria-label={`Remove ${f.name}`}
              onClick={() => onRemoveFile(i)}
              disabled={uploading}
              className="hover:text-primary grid size-5 place-items-center rounded disabled:opacity-40"
            >
              <X className="size-3.5" />
            </button>
          )}
        </span>
      ))}
    </div>
  )

  // Bottom toolbar — tools/attach on the left, send/stop on the right. Shared by
  // both variants and pinned BELOW the input, so the controls never move as the
  // textarea grows (matches the Perplexity composer).
  const controls = (
    <div className="flex items-center justify-between gap-2">
      <div className="flex items-center gap-1">
        {attachButton}
        {toolSelector}
      </div>
      {primaryButton}
    </div>
  )

  // Both variants STACK the input over the toolbar so a tall, multi-line entry
  // pushes the box DOWN while every control stays put and all text stays in
  // view. They differ only in their container: the PILL fills the morphing
  // surface (which paints its own rounded background + ring), the BAR brings
  // its own rounded panel for the docked footer.
  const stack = (
    <>
      {chips}
      <div className="px-1">{textareaEl}</div>
      {controls}
    </>
  )

  if (pill) {
    return <div className="flex w-full flex-col gap-1.5 p-2.5">{stack}</div>
  }

  return (
    <div className="relative w-full">
      <div className="flex flex-col gap-1.5 rounded-3xl bg-zinc-100 p-2.5 dark:bg-zinc-800">
        {stack}
      </div>
    </div>
  )
}
