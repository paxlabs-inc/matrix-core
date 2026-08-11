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
import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'
import {
  ArrowUp,
  Check,
  Code,
  Coins,
  FileText,
  Globe,
  ImageIcon,
  Lightbulb,
  MicIcon,
  MicOffIcon,
  Music2Icon,
  Paperclip,
  Plus,
  BrainIcon,
  SquareIcon,
  VideoIcon,
  Wallet,
  X,
} from '@/lib/matrix-icons'
import { DropdownMenu, DropdownMenuItem } from '@astryxdesign/core/DropdownMenu'
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
    icon: BrainIcon,
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
  voiceActive = false,
  voiceStateLabel,
  voiceNotice,
  voiceDevices = [],
  voiceDeviceId = '',
  onVoiceDeviceChange,
  onVoiceToggle,
  voiceStartLabel = 'Start voice',
  voiceStopLabel = 'Stop voice',
  voiceMicrophoneLabel = 'Microphone',
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
  voiceActive?: boolean
  voiceStateLabel?: string
  voiceNotice?: string
  voiceDevices?: MediaDeviceInfo[]
  voiceDeviceId?: string
  onVoiceDeviceChange?: (deviceId: string) => void
  onVoiceToggle?: () => void
  voiceStartLabel?: string
  voiceStopLabel?: string
  voiceMicrophoneLabel?: string
}) {
  const pill = variant === 'pill'
  const active = neoModeDef(mode)
  const ActiveIcon = active.icon

  const internalRef = useRef<HTMLTextAreaElement>(null)
  const taRef = inputRef ?? internalRef
  const imageFileRef = useRef<HTMLInputElement>(null)
  const videoFileRef = useRef<HTMLInputElement>(null)
  const generalFileRef = useRef<HTMLInputElement>(null)
  const [expanded, setExpanded] = useState(false)

  // Auto-grow: reset to single-row, then expand to content up to the cap so the
  // composer behaves like Grok's multi-line input across every screen size.
  useEffect(() => {
    const el = taRef.current
    if (!el) return
    el.style.height = 'auto'
    const nextHeight = Math.min(el.scrollHeight, MAX_TEXTAREA_HEIGHT)
    el.style.height = `${nextHeight}px`
    setExpanded((wasExpanded) =>
      value.length === 0 ? false : wasExpanded || value.includes('\n') || nextHeight > 40,
    )
  }, [value, taRef])

  const pickMode = useCallback(
    (m: NeoMode) => {
      onModeChange(m)
      // Keep the composer hot so the user can type the argument right away.
      window.setTimeout(() => taRef.current?.focus(), 0)
    },
    [onModeChange, taRef],
  )

  // Quiet text controls mirror the reference composer instead of recreating
  // the old circular product toolbar inside the Astryx shell.
  const toolSelector = (
    <DropdownMenu
      button={{
        label: active.label,
        icon: <ActiveIcon className="size-3.5 shrink-0" />,
        variant: 'ghost',
        size: 'sm',
        tooltip: `Tool: ${active.label}`,
      }}
      hasChevron
      placement={pill ? 'below' : 'above'}
      menuWidth={256}
    >
      {NEO_MODES.map((m) => {
        const Icon = m.icon
        const selected = m.id === mode
        return (
          <DropdownMenuItem
            key={m.id}
            onClick={() => pickMode(m.id)}
            icon={<Icon className="size-4" />}
            label={m.label}
            description={m.hint}
            endContent={selected ? <Check className="size-4 shrink-0" /> : undefined}
          />
        )
      })}
    </DropdownMenu>
  )

  const stagePickedFiles = (files: FileList | null) => {
    const picked = Array.from(files ?? [])
    if (picked.length) onAddFiles?.(picked)
  }

  // Attach — the visible chooser is typed and explicit, but all three native
  // pickers feed the same existing staging/upload pipeline.
  const attachButton = onAddFiles && (
    <>
      <input
        ref={imageFileRef}
        type="file"
        accept="image/*"
        multiple
        aria-label="Choose images"
        className="hidden"
        onChange={(e) => {
          stagePickedFiles(e.target.files)
          e.target.value = ''
        }}
      />
      <input
        ref={videoFileRef}
        type="file"
        accept="video/*"
        multiple
        aria-label="Choose videos"
        className="hidden"
        onChange={(e) => {
          stagePickedFiles(e.target.files)
          e.target.value = ''
        }}
      />
      <input
        ref={generalFileRef}
        type="file"
        multiple
        aria-label="Choose files"
        className="hidden"
        onChange={(e) => {
          stagePickedFiles(e.target.files)
          e.target.value = ''
        }}
      />
      <DropdownMenu
        button={{
          label: 'Add an attachment',
          icon: <Plus className="size-[1.1rem]" />,
          variant: 'ghost',
          size: 'sm',
          isIconOnly: true,
          isDisabled: uploading,
          tooltip: 'Add an image, video, or file',
        }}
        placement={pill ? 'below' : 'above'}
        menuWidth={288}
      >
        <AttachmentChoice
          icon={ImageIcon}
          label="Image"
          hint="Photos, screenshots, and artwork"
          onSelect={() => imageFileRef.current?.click()}
        />
        <AttachmentChoice
          icon={VideoIcon}
          label="Video"
          hint="Video clips from your device"
          onSelect={() => videoFileRef.current?.click()}
        />
        <AttachmentChoice
          icon={FileText}
          label="File"
          hint="Documents, audio, archives, or other files"
          onSelect={() => generalFileRef.current?.click()}
        />
        <p className="text-muted-foreground px-2 py-2 text-[0.68rem] leading-relaxed">
          You can choose multiple items. Files stay staged until you send.
        </p>
      </DropdownMenu>
    </>
  )

  const voiceButton = onVoiceToggle && (
    <button
      type="button"
      aria-label={voiceActive ? voiceStopLabel : voiceStartLabel}
      title={voiceActive ? voiceStopLabel : voiceStartLabel}
      aria-pressed={voiceActive}
      onClick={onVoiceToggle}
      disabled={uploading}
      className={cn(
        'group flex shrink-0 items-center justify-center rounded-full transition-colors disabled:opacity-60',
        'size-8',
        voiceActive
          ? 'bg-primary text-primary-foreground'
          : 'text-muted-foreground hover:bg-surface-hover hover:text-foreground',
      )}
    >
      {voiceActive ? <MicOffIcon className="size-[17px]" /> : <MicIcon className="size-[17px]" />}
    </button>
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
      className="neo-input text-foreground placeholder:text-muted-foreground min-h-8 w-full min-w-0 flex-1 resize-none bg-transparent py-1 text-[0.95rem] leading-6 font-normal outline-none disabled:opacity-60"
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
        className={cn(
          'bg-accent text-foreground hover:bg-surface-hover flex shrink-0 items-center justify-center rounded-full transition-colors',
          'size-8 min-w-8',
        )}
      >
        <SquareIcon className="size-3.5 fill-current" />
      </button>
    ) : (
      <button
        type="button"
        onClick={onSubmit}
        aria-label="Send"
        disabled={uploading}
        className={cn(
          'bg-foreground text-background hover:bg-foreground/90 disabled:bg-accent disabled:text-muted-foreground flex shrink-0 items-center justify-center rounded-full transition-colors disabled:cursor-not-allowed',
          'size-8 min-w-8',
        )}
      >
        <ArrowUp className="size-[17px]" />
      </button>
    )

  // Removable chips for the staged uploads, rendered in Astryx's composer
  // drawer above the input.
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

  const drawer = (voiceActive || voiceNotice || attachments.length > 0) && (
    <div className="flex flex-col gap-2 px-1 pt-1 pb-2">
      {(voiceActive || voiceNotice) && (
        <div className="bg-card flex items-center gap-2 rounded-xl px-3 py-2 text-xs">
          {voiceActive && (
            <span className="bg-primary size-1.5 shrink-0 animate-pulse rounded-full" />
          )}
          <span className="text-muted-foreground min-w-0 flex-1">
            {voiceNotice || voiceStateLabel}
          </span>
          {voiceActive && voiceDevices.length > 1 && (
            <>
              <label className="sr-only" htmlFor="neo-voice-microphone">
                {voiceMicrophoneLabel}
              </label>
              <select
                id="neo-voice-microphone"
                value={voiceDeviceId}
                onChange={(event) => onVoiceDeviceChange?.(event.target.value)}
                aria-label={voiceMicrophoneLabel}
                className="bg-card text-foreground max-w-40 rounded-lg px-2 py-1 outline-none"
              >
                {voiceDevices.map((device) => (
                  <option key={device.deviceId} value={device.deviceId}>
                    {device.label || voiceMicrophoneLabel}
                  </option>
                ))}
              </select>
            </>
          )}
        </div>
      )}
      {chips}
    </div>
  )

  return (
    <div
      className={cn(
        'bg-muted w-full px-2 py-2',
        pill ? 'rounded-[1.75rem]' : 'rounded-3xl',
        (expanded || voiceActive || attachments.length > 0) && 'rounded-2xl',
      )}
      data-neo-composer
      data-variant={variant}
      data-layout={expanded ? 'expanded' : 'compact'}
    >
      {drawer}
      <div className="grid min-h-9 min-w-0 grid-cols-[auto_minmax(0,1fr)_auto] items-end gap-x-1">
        <div
          className={cn(
            'flex h-8 shrink-0 items-center',
            expanded ? 'col-start-1 row-start-2' : 'col-start-1 row-start-1',
          )}
        >
          {attachButton}
        </div>
        <div
          className={cn(
            'flex min-w-0 items-end',
            expanded
              ? 'col-span-3 col-start-1 row-start-1 px-2 pb-1'
              : 'col-start-2 row-start-1 px-1',
          )}
        >
          {textareaEl}
        </div>
        <div
          className={cn(
            'flex h-8 shrink-0 items-center gap-0.5',
            expanded ? 'col-start-3 row-start-2' : 'col-start-3 row-start-1',
          )}
        >
          {toolSelector}
          {voiceButton}
          {primaryButton}
        </div>
      </div>
    </div>
  )
}

function AttachmentChoice({
  icon: Icon,
  label,
  hint,
  onSelect,
}: {
  icon: IconType
  label: string
  hint: string
  onSelect: () => void
}) {
  return (
    <DropdownMenuItem
      onClick={() => window.setTimeout(onSelect, 0)}
      icon={<Icon className="size-4" />}
      label={label}
      description={hint}
    />
  )
}
