'use client'

/**
 * NeoSurface — the Adaptive Surface.
 *
 * The user has two authority primitives: INTENT (type/speak — also the mid-run
 * interrupt) and CONSENT (confirm irreversible money). At true rest the surface
 * is a single centered input PILL. The moment a conversation exists it becomes a
 * full-height chat that uses all available space — the answer is the product, so
 * it is never boxed into a small floating card.
 *
 * Agent ACTION lives in its OWN pane: when Neo actually works — a web search,
 * tool / MCL-pipeline steps, an Agent Swarm, generated media — the chat splits
 * into a two-pane workspace and "Neo's Computer" (the NeoComputer panel) opens
 * on the right as a live window into the agent at work. The conversation rail
 * stays a clean chat thread on the left. The panel can be collapsed at any time
 * (a status chip above the composer reopens it); a plain chat reply never opens
 * it. Fused to the client design system — separation by TONE only (no
 * borders/shadows/glow), single accent #004ced, rounded brand font.
 */
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import Image from 'next/image'
import { Link } from '@/i18n/navigation'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { toast } from 'sonner'
import { useTranslations } from 'next-intl'
import {
  Activity,
  BrainIcon,
  ChevronDown,
  ChevronRight,
  Code,
  Coins,
  Cpu,
  EyeOffIcon,
  FileIcon,
  Globe,
  ImageIcon,
  MessageSquare,
  Monitor,
  PanelLeftIcon,
  Plus,
  Search,
  Settings,
  SquareIcon,
  Wallet,
} from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'
import { uploadMedia, mediaKindForMime } from '@/lib/api/media'
import { NeoMediaGrid, NeoMediaSkeleton } from '@/components/matrix/neo/neo-media'
import { haltAll } from '@/lib/api/runs'
import { getSession } from '@/lib/auth/session'
import { usePrefs } from '@/lib/prefs'
import { useVoiceSession } from '@/hooks/api/useVoiceSession'
import type { ConversationSummary } from '@/lib/api/conversations'
import type { ChatMessage, ChatPhase, NeoTask, PendingGate } from '@/hooks/api/useChat'
import type { AskResponse } from '@/lib/construct/types.gen'
import { PixelGrid, WaveBars } from '@/components/matrix/cody/loaders'
import { NeoComputer } from '@/components/matrix/neo/neo-computer'
import { WalletApproval } from '@/components/matrix/neo/wallet-approval'
import { NeoComposer, composeNeoMessage, type NeoMode } from '@/components/matrix/neo/neo-composer'
import { Sheet, SheetContent, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import {
  NeoAssistantMessage,
  NeoLiveTurn,
  NeoUserMessage,
} from '@/components/matrix/neo/neo-message'

const EASE = [0.32, 0.72, 0, 1] as const
const DONE_COLOR = 'oklch(0.72 0.14 155)' // the surface's "ready/success" green

// Idle quick actions — one tap arms a real composer tool (or pre-fills a
// real prompt) and focuses the input, so the user reviews then sends. These
// map 1:1 onto NeoComposer's actual modes; nothing here is decorative.
const NEO_QUICK: { label: string; icon: typeof Globe; mode?: NeoMode; prompt?: string }[] = [
  { label: 'Code', icon: Code, mode: 'code' },
  { label: 'Research', icon: Globe, mode: 'web' },
  { label: 'Images', icon: ImageIcon, mode: 'image' },
  { label: 'PAX', icon: Coins, mode: 'price' },
]

function NeoMark({ className }: { className?: string }) {
  return (
    <span
      aria-hidden
      className={cn('bg-primary grid shrink-0 place-items-center rounded-[28%]', className)}
    >
      <span className="bg-background block size-[38%] rounded-[2px]" />
    </span>
  )
}

/** NeoStatusMark — Neo's persistent live-status mark, rendered under the
 *  latest turn in the thread: Pixel Grid while idle (no task, not
 *  responding), Wave Bars while a run is thinking/working. */
function NeoStatusMark({ live }: { live: boolean }) {
  return live ? <WaveBars size={26} bars={4} /> : <PixelGrid size={26} />
}

function StatePill({
  phase,
  hasTask,
  resuming,
  connectionRetrying,
}: {
  phase: ChatPhase
  hasTask: boolean
  /** F2 — F1 reattach is mid-replay; render plain-language reconnecting copy. */
  resuming?: boolean
  /** F2 — non-terminal stream drop is being retried in the background. */
  connectionRetrying?: boolean
}) {
  const live = phase === 'thinking' || phase === 'working'
  // F2 — connectionRetrying wins (most acute), then resuming, then existing.
  // Plain language only (ux_truth): no "SSE", no "broker", no "topic".
  const label = connectionRetrying
    ? 'Connection lost — retrying…'
    : resuming
      ? 'Reconnecting to your task…'
      : phase === 'thinking'
        ? 'thinking'
        : phase === 'working'
          ? 'working'
          : hasTask
            ? 'ready'
            : 'idle'
  const pulse = live || resuming || connectionRetrying
  return (
    <span className="bg-card text-muted-foreground hidden items-center gap-2 rounded-full px-3 py-1.5 font-mono text-[0.7rem] tracking-wide sm:flex">
      <span
        className={cn(
          'size-1.5 rounded-full',
          pulse ? 'bg-primary animate-pulse' : 'bg-muted-foreground/50',
        )}
        style={!pulse && hasTask ? { background: 'oklch(0.72 0.14 155)' } : undefined}
      />
      {label}
    </span>
  )
}

function ChromeButton({
  onClick,
  label,
  children,
}: {
  onClick: () => void
  label: string
  children: ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      className="text-muted-foreground hover:bg-card hover:text-foreground grid size-9 place-items-center rounded-full transition"
    >
      {children}
    </button>
  )
}

function RailButton({
  label,
  onClick,
  children,
}: {
  label: string
  onClick?: () => void
  children: ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 place-items-center rounded-xl transition"
    >
      {children}
    </button>
  )
}

/** A labeled navigation row in the expanded sidebar. */
function SidebarLink({
  icon: Icon,
  label,
  onClick,
}: {
  icon: typeof Globe
  label: string
  onClick?: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="text-muted-foreground hover:bg-muted/60 hover:text-foreground flex w-full items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-left text-[0.8125rem] font-medium transition-colors"
    >
      <Icon className="size-4 shrink-0 opacity-80" />
      {label}
    </button>
  )
}

/** Shared navigation props for the sidebar (desktop rail + mobile drawer). */
type SidebarNavProps = {
  conversations: ConversationSummary[]
  activeConversationId?: string | null
  onNewChat: () => void
  onSelectConversation?: (id: string) => void
  onOpenHistory?: () => void
  /** Open the Timeline page (Neo's exposed, read-only memory). */
  onOpenTimeline?: () => void
  /** Open the Workspace / Files page. */
  onOpenFiles?: () => void
  /** Open the Self-Model page (agent's structural self-knowledge). */
  onOpenSelfModel?: () => void
  onOpenSettings?: () => void
  /** Open the agent Wallet page (smart wallet leash + LayerX account). */
  onOpenWallet?: () => void
}

/**
 * SidebarInner — the shared sidebar body (CTA, navigation, the REAL recent-task
 * list, and the account row). Reused verbatim by the desktop rail and the
 * mobile drawer so both stay in lockstep. `onNavigate` lets the mobile drawer
 * close itself after any selection.
 */
function SidebarInner({
  conversations,
  activeConversationId,
  onNewChat,
  onSelectConversation,
  onOpenHistory,
  onOpenTimeline,
  onOpenFiles,
  onOpenSelfModel,
  onOpenSettings,
  onOpenWallet,
  onNavigate,
}: SidebarNavProps & { onNavigate?: () => void }) {
  const [email, setEmail] = useState<string | null>(null)
  // Locally archived (hidden) conversation ids — soft-delete, UI only.
  const [archivedIds, setArchivedIds] = useState<Set<string>>(() => new Set())
  // Whether the Tasks section is collapsed.
  const [tasksCollapsed, setTasksCollapsed] = useState(false)

  // Real account identity — the signed-in user's email (or null when auth is
  // not configured / anonymous dev). Never a placeholder.
  useEffect(() => {
    let alive = true
    getSession()
      .then((s) => {
        if (alive) setEmail(s?.user?.email ?? null)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [])

  const accountInitial = (email?.trim()?.[0] ?? 'U').toUpperCase()
  // Run the action, then let a mobile drawer dismiss itself.
  const go = (fn?: () => void) => () => {
    fn?.()
    onNavigate?.()
  }
  const toggleArchive = (id: string) =>
    setArchivedIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  const visibleConversations = conversations.filter((c) => !archivedIds.has(c.conversation_id))

  return (
    <>
      {/* primary CTA */}
      <div className="px-3">
        <button
          type="button"
          onClick={go(onNewChat)}
          className="bg-primary text-primary-foreground hover:bg-primary/90 flex w-full items-center gap-2 rounded-xl px-3 py-2 text-[0.8125rem] font-medium transition-colors"
        >
          <Plus className="size-[0.95rem]" />
          New task
        </button>
      </div>

      {/* navigation */}
      <div className="mt-3 flex flex-col gap-0.5 px-3">
        {onOpenHistory && <SidebarLink icon={Search} label="Search" onClick={go(onOpenHistory)} />}
        {onOpenTimeline && (
          <SidebarLink icon={BrainIcon} label="Timeline" onClick={go(onOpenTimeline)} />
        )}
        {onOpenFiles && <SidebarLink icon={FileIcon} label="Workspace" onClick={go(onOpenFiles)} />}
        {onOpenSelfModel && (
          <SidebarLink icon={Cpu} label="Self-Model" onClick={go(onOpenSelfModel)} />
        )}
        {onOpenWallet && <SidebarLink icon={Wallet} label="Wallet" onClick={go(onOpenWallet)} />}
        <Link
          href="/explorer"
          onClick={onNavigate}
          className="text-muted-foreground hover:bg-muted/60 hover:text-foreground flex w-full items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-left text-[0.8125rem] font-medium transition-colors"
        >
          <Activity className="size-[1.05rem] shrink-0" />
          Explorer
        </Link>
        <Link
          href="/cody"
          onClick={onNavigate}
          className="text-muted-foreground hover:bg-muted/60 hover:text-foreground flex w-full items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-left text-[0.8125rem] font-medium transition-colors"
        >
          <Image
            src="/logomatrix_dim.png"
            alt=""
            width={17}
            height={17}
            className="shrink-0 rounded-sm opacity-90"
          />
          Cody Code
        </Link>
        <Link
          href="/code"
          onClick={onNavigate}
          className="text-muted-foreground hover:bg-muted/60 hover:text-foreground flex w-full items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-left text-[0.8125rem] font-medium transition-colors"
        >
          <Code className="size-4 shrink-0 opacity-80" />
          Templates
        </Link>
      </div>

      {/* real recent-tasks list */}
      <div className="mt-3 flex min-h-0 flex-1 flex-col px-3">
        <button
          type="button"
          onClick={() => setTasksCollapsed((v) => !v)}
          className="text-muted-foreground/50 hover:text-muted-foreground flex items-center gap-1 px-2.5 pb-1.5 text-[0.65rem] font-medium tracking-[0.09em] uppercase transition-colors"
        >
          {tasksCollapsed ? (
            <ChevronRight className="size-3" />
          ) : (
            <ChevronDown className="size-3" />
          )}
          Tasks
          {visibleConversations.length > 0 && (
            <span className="ml-auto opacity-60">{visibleConversations.length}</span>
          )}
        </button>
        {!tasksCollapsed && (
          <div data-sidebar="content" className="-mr-1 min-h-0 flex-1 overflow-y-auto pr-1">
            {visibleConversations.length === 0 ? (
              <div className="text-muted-foreground/60 flex flex-col items-center gap-2 px-2 py-8 text-center">
                <MessageSquare className="size-5 opacity-60" />
                <p className="text-xs">
                  {archivedIds.size > 0 ? 'All tasks archived' : 'No tasks yet'}
                </p>
                {archivedIds.size > 0 && (
                  <button
                    type="button"
                    onClick={() => setArchivedIds(new Set())}
                    className="text-muted-foreground hover:text-foreground text-[0.7rem] underline underline-offset-2"
                  >
                    Restore all
                  </button>
                )}
              </div>
            ) : (
              <ul className="flex flex-col gap-0.5">
                {visibleConversations.map((c) => {
                  const on = c.conversation_id === activeConversationId
                  return (
                    <li key={c.conversation_id} className="group/task relative">
                      <button
                        type="button"
                        onClick={go(() => onSelectConversation?.(c.conversation_id))}
                        title={c.title || 'Untitled task'}
                        className={cn(
                          'flex w-full items-center gap-2 rounded-lg px-2.5 py-1.5 text-left text-[0.8125rem] transition-colors',
                          on
                            ? 'bg-muted text-foreground'
                            : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
                        )}
                      >
                        <MessageSquare className="size-4 shrink-0 opacity-70" />
                        <span className="min-w-0 flex-1 truncate">
                          {c.title || 'Untitled task'}
                        </span>
                      </button>
                      <button
                        type="button"
                        onClick={(e) => {
                          e.stopPropagation()
                          toggleArchive(c.conversation_id)
                        }}
                        title="Archive task"
                        aria-label="Archive task"
                        className="text-muted-foreground/50 hover:text-foreground hover:bg-muted/60 absolute top-1 right-1 grid size-6 place-items-center rounded-md opacity-0 transition group-hover/task:opacity-100"
                      >
                        <EyeOffIcon className="size-3" />
                      </button>
                    </li>
                  )
                })}
              </ul>
            )}
          </div>
        )}
      </div>

      {/* account */}
      <div className="mt-auto px-3 pt-2 pb-3">
        <button
          type="button"
          onClick={go(onOpenSettings)}
          className="hover:bg-muted/60 flex w-full items-center gap-2.5 rounded-xl px-2 py-2 text-left transition-colors"
        >
          <span className="bg-muted text-foreground grid size-8 shrink-0 place-items-center rounded-full text-xs font-semibold">
            {accountInitial}
          </span>
          <span className="min-w-0 flex-1">
            <span className="text-foreground block truncate text-sm font-medium">
              {email ?? 'Your account'}
            </span>
            <span className="text-muted-foreground/70 block text-[0.7rem]">
              Free preview 10 PAX
            </span>
          </span>
          <Settings className="text-muted-foreground/70 size-4 shrink-0" />
        </button>
      </div>
    </>
  )
}

/**
 * NeoSidebar — the persistent left sidebar (md+ only). Brand + collapse at the
 * top, then the shared SidebarInner body. Tone-only against the `bg-background`
 * stage; collapses to a slim icon rail. On phones it stays hidden — the mobile
 * drawer (NeoMobileSidebar) carries the same body behind the header hamburger.
 */
function NeoSidebar(props: SidebarNavProps) {
  const {
    onNewChat,
    onOpenHistory,
    onOpenTimeline,
    onOpenFiles,
    onOpenSelfModel,
    onOpenSettings,
    onOpenWallet,
  } = props
  const [collapsed, setCollapsed] = useState(false)

  if (collapsed) {
    return (
      <nav className="bg-card relative z-30 hidden w-[3.75rem] shrink-0 flex-col items-center gap-1.5 py-4 md:flex">
        <button
          type="button"
          onClick={() => setCollapsed(false)}
          aria-label="Expand sidebar"
          title="Expand sidebar"
          className="mb-3"
        >
          <NeoMark className="size-7" />
        </button>
        <RailButton label="New task" onClick={onNewChat}>
          <Plus className="size-[1.2rem]" />
        </RailButton>
        {onOpenHistory && (
          <RailButton label="Search tasks" onClick={onOpenHistory}>
            <Search className="size-[1.05rem]" />
          </RailButton>
        )}
        {onOpenTimeline && (
          <RailButton label="Timeline" onClick={onOpenTimeline}>
            <BrainIcon className="size-[1.05rem]" />
          </RailButton>
        )}
        {onOpenFiles && (
          <RailButton label="Workspace" onClick={onOpenFiles}>
            <FileIcon className="size-[1.05rem]" />
          </RailButton>
        )}
        {onOpenSelfModel && (
          <RailButton label="Self-Model" onClick={onOpenSelfModel}>
            <Cpu className="size-[1.05rem]" />
          </RailButton>
        )}
        {onOpenWallet && (
          <RailButton label="Wallet" onClick={onOpenWallet}>
            <Wallet className="size-[1.05rem]" />
          </RailButton>
        )}
        <Link
          href="/explorer"
          aria-label="Explorer"
          title="Explorer"
          className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 place-items-center rounded-xl transition"
        >
          <Activity className="size-[1.05rem]" />
        </Link>
        <Link
          href="/cody"
          aria-label="Cody Code"
          title="Cody Code"
          className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 place-items-center rounded-xl transition"
        >
          <Image src="/logomatrix_dim.png" alt="" width={20} height={20} className="rounded-sm" />
        </Link>
        <Link
          href="/code"
          aria-label="Templates"
          title="Templates"
          className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 place-items-center rounded-xl transition"
        >
          <Code className="size-[1.05rem]" />
        </Link>
        {onOpenSettings && (
          <span className="mt-auto">
            <RailButton label="Settings" onClick={onOpenSettings}>
              <Settings className="size-[1.05rem]" />
            </RailButton>
          </span>
        )}
      </nav>
    )
  }

  return (
    <nav className="bg-card relative z-30 hidden w-64 shrink-0 flex-col md:flex">
      {/* brand + collapse */}
      <div className="flex items-center gap-2 px-4 py-4">
        <NeoMark className="size-7" />
        <span className="text-foreground text-sm font-semibold tracking-tight">Matrix</span>
        <button
          type="button"
          onClick={() => setCollapsed(true)}
          aria-label="Collapse sidebar"
          title="Collapse sidebar"
          className="text-muted-foreground hover:bg-muted hover:text-foreground ml-auto grid size-7 place-items-center rounded-lg transition"
        >
          <PanelLeftIcon className="size-[1.05rem]" />
        </button>
      </div>

      <SidebarInner {...props} />
    </nav>
  )
}

/**
 * NeoMobileSidebar — the phone drawer. Renders the SAME SidebarInner body inside
 * a left slide-in Sheet, opened from the surface header hamburger. Tone-only
 * (bg-card panel, no border stroke for depth); any selection dismisses it.
 */
function NeoMobileSidebar({
  open,
  onOpenChange,
  ...nav
}: SidebarNavProps & { open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="left"
        showCloseButton={false}
        className="bg-card flex w-72 flex-col gap-0 border-0 p-0"
      >
        <SheetHeader className="flex-row items-center gap-2 px-4 py-4">
          <NeoMark className="size-7" />
          <SheetTitle className="text-foreground text-sm font-semibold tracking-tight">
            Matrix
          </SheetTitle>
        </SheetHeader>
        <SidebarInner {...nav} onNavigate={() => onOpenChange(false)} />
      </SheetContent>
    </Sheet>
  )
}

export function NeoSurface({
  phase,
  task,
  messages,
  send,
  pendingGate,
  resuming,
  connectionRetrying,
  answerGate,
  respondAsk,
  dismissTask,
  conversations = [],
  conversationId,
  onVoiceIntent,
  onSelectConversation,
  onNewChat,
  onOpenHistory,
  onOpenTimeline,
  onOpenFiles,
  onOpenSelfModel,
  onOpenSettings,
  onOpenWallet,
  embedded = false,
}: {
  phase: ChatPhase
  task: NeoTask | null
  /** The durable conversation thread (user turns + settled answers). */
  messages: ChatMessage[]
  send: (text: string) => void
  pendingGate?: PendingGate | null
  /** Every persisted thread (newest-first) — the sidebar tasks list. */
  conversations?: ConversationSummary[]
  /** The currently open thread id (highlights its sidebar row). */
  conversationId?: string | null
  /** Follow a run created by the room-bound voice worker. */
  onVoiceIntent?: (intentId: string) => void
  /** Reopen a past thread from the sidebar tasks list. */
  onSelectConversation?: (id: string) => void
  /** F2 — durable live-run resume visible state: surface renders
   *  "Reconnecting to your task…" until the first event lands. */
  resuming?: boolean
  /** F2 — visible "Connection lost — retrying…" during a non-terminal
   *  stream drop while the bounded re-subscribe loop is in flight. */
  connectionRetrying?: boolean
  answerGate: (approved: boolean, answer?: string) => void
  /** Answer a parked Construct Ask (the bidirectional back-channel). */
  respondAsk?: (surfaceId: string, response: AskResponse) => void
  dismissTask: () => void
  onNewChat?: () => void
  onOpenHistory?: () => void
  /** Opens the Timeline page — Neo's exposed, read-only memory. */
  onOpenTimeline?: () => void
  /** Opens the Workspace / Files page. */
  onOpenFiles?: () => void
  /** Opens the Self-Model page (agent's structural self-knowledge). */
  onOpenSelfModel?: () => void
  /** Opens the settings slide-over (account, preferences, legal). */
  onOpenSettings?: () => void
  /** Opens the agent Wallet page (smart wallet leash + LayerX account). */
  onOpenWallet?: () => void
  /** Mounted inside the Construct OS shell as the narration panel. The shell's
   *  environment stage is the centerpiece that renders Neo's work (the relocated
   *  "Neo's Computer" + the Construct surfaces), so this surface drops its own
   *  in-thread work pane and stays a pure chat thread. */
  embedded?: boolean
}) {
  const t = useTranslations('agentChat')
  const reduce = useReducedMotion()
  const [value, setValue] = useState('')
  const [mode, setMode] = useState<NeoMode>('auto')
  const [userName, setUserName] = useState<string | null>(null)
  const [prefs] = usePrefs()
  const voice = useVoiceSession({
    conversationId,
    settings: prefs.voice,
    onIntent: onVoiceIntent,
    unavailableNotice: t('voiceUnavailable'),
    firstTurnNotice: t('voiceNeedsConversation'),
  })
  const voiceStateLabel =
    voice.state === 'listening'
      ? t('voiceListening')
      : voice.state === 'thinking'
        ? t('voiceThinking')
        : voice.state === 'speaking'
          ? t('voiceSpeaking')
          : t('voiceConnecting')

  // Fetch the signed-in user's display name for the idle greeting.
  useEffect(() => {
    let alive = true
    getSession()
      .then((s) => {
        if (!alive || !s?.user) return
        const meta = s.user.user_metadata ?? {}
        const name = meta.full_name ?? meta.name ?? meta.preferred_username ?? null
        setUserName(name ?? s.user.email?.split('@')[0] ?? null)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [])
  // Files staged in the composer, awaiting upload to the agent volume on send.
  const [files, setFiles] = useState<File[]>([])
  const [uploading, setUploading] = useState(false)
  // Whether "Neo's Computer" (the right work pane) is open. Auto-opens on the
  // rising edge of real work (desktop only); collapsible at any time.
  const [computerOpen, setComputerOpen] = useState(false)
  // Whether the mobile sidebar drawer is open (phones only; the desktop rail is
  // always present on md+).
  const [mobileSidebarOpen, setMobileSidebarOpen] = useState(false)
  const prevWorkRef = useRef(false)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  const cardInputRef = useRef<HTMLTextAreaElement>(null)
  const bodyRef = useRef<HTMLDivElement>(null)

  const hasThread = messages.length > 0
  const gated = !!pendingGate
  const live = phase === 'thinking' || phase === 'working'
  // A run that has not yet settled — the only state a dismiss/abort acts on.
  const runLive = !!task && !task.done
  // In the shell, the work centerpiece is the environment stage, so the
  // in-thread "Neo's Computer" pane is suppressed here (it relocated, it did
  // not disappear). The chat thread stays a pure narration panel.
  const showComputer = !embedded
  // Once a conversation exists the surface is a full-height chat that uses all
  // available space; the centered idle pill is only shown at true rest.
  const conversation = hasThread || phase !== 'idle' || !!task || gated

  // Esc aborts the active run — never bypasses a consent gate, never nukes the
  // thread when nothing is running.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && runLive && !gated) dismissTask()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [runLive, gated, dismissTask])

  // Focus the idle pill at rest; focus the docked composer once a conversation
  // is up (and again when a task settles) so a follow-up can be typed at once.
  const taskDone = task?.done
  useEffect(() => {
    if (conversation && gated) return
    const ref = conversation ? cardInputRef : inputRef
    const id = window.setTimeout(() => ref.current?.focus(), 60)
    return () => window.clearTimeout(id)
  }, [conversation, gated, taskDone])

  // ── Stick-to-bottom scrolling ──────────────────────────────────────────────
  // The reader owns the scroll position. A new USER turn always snaps to the
  // latest message; the agent's streaming output only follows when the reader is
  // already parked at the bottom — scrolling up to re-read never gets yanked
  // back down. A jump-to-bottom affordance appears whenever they're scrolled up.
  const stickRef = useRef(true)
  const prevLenRef = useRef(messages.length)
  const [showJump, setShowJump] = useState(false)

  const scrollToBottom = useCallback((behavior: ScrollBehavior = 'auto') => {
    const el = bodyRef.current
    if (!el) return
    el.scrollTo({ top: el.scrollHeight, behavior })
    stickRef.current = true
    setShowJump(false)
  }, [])

  const onBodyScroll = useCallback(() => {
    const el = bodyRef.current
    if (!el) return
    const dist = el.scrollHeight - el.scrollTop - el.clientHeight
    const atBottom = dist < 96
    stickRef.current = atBottom
    setShowJump(!atBottom)
  }, [])

  // A new user turn (or a freshly loaded thread) snaps to the latest message.
  const lastRole = messages.length > 0 ? messages[messages.length - 1].role : null
  useEffect(() => {
    const grew = messages.length > prevLenRef.current
    prevLenRef.current = messages.length
    if (grew && (lastRole === 'user' || stickRef.current)) {
      requestAnimationFrame(() => scrollToBottom('auto'))
    }
  }, [messages.length, lastRole, scrollToBottom])

  // Follow live streaming output, but only while parked at the bottom.
  useEffect(() => {
    if (!stickRef.current) return
    const el = bodyRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [
    task?.steps.length,
    task?.searches.length,
    task?.media.length,
    task?.artifacts.length,
    task?.surfaces.length,
    task?.swarm?.agents.length,
    task?.answer,
    task?.streamingAnswer,
    task?.thinking,
    gated,
  ])

  const addFiles = useCallback((picked: File[]) => {
    setFiles((prev) => [...prev, ...picked])
  }, [])
  const removeFile = useCallback((index: number) => {
    setFiles((prev) => prev.filter((_, i) => i !== index))
  }, [])

  const submit = useCallback(async () => {
    if (uploading) return
    const raw = value.trim()
    // `auto` needs a prompt; a picked tool can fire its canned action with no
    // argument, and an attachment alone is a valid turn (e.g. “transcribe this”).
    if (!raw && files.length === 0 && mode === 'auto') return
    let out = composeNeoMessage(mode, raw)
    // Upload each staged file to the agent's machine volume, then embed its
    // /media reference so Neo can edit / animate / transcribe it.
    if (files.length > 0) {
      setUploading(true)
      try {
        const markers: string[] = []
        for (const f of files) {
          const up = await uploadMedia(f, f.name)
          const kind = up.kind !== 'file' ? up.kind : mediaKindForMime(f.type)
          // Documents keep their original filename in the marker so both the
          // user bubble and the model know what /media/<id>.<ext> actually is.
          markers.push(
            kind === 'file'
              ? `[attached file: ${up.url} (${f.name.replace(/[[\]()]/g, '_')})]`
              : `[attached ${kind}: ${up.url}]`,
          )
        }
        out = [out, markers.join('\n')].filter(Boolean).join('\n\n')
      } catch {
        toast.error(t('uploadError'))
        setUploading(false)
        return
      }
      setUploading(false)
      setFiles([])
    }
    if (!out.trim()) return
    send(out)
    setValue('')
  }, [uploading, value, files, mode, send, t])

  // Arm a quick action: set the composer's real tool mode and/or pre-fill a
  // prompt, then focus the input so the user reviews and sends. Maps 1:1 onto
  // the composer's actual modes — nothing here is cosmetic.
  const applyQuickAction = useCallback((q: { mode?: NeoMode; prompt?: string }) => {
    setMode(q.mode ?? 'auto')
    if (q.prompt !== undefined) setValue(q.prompt)
    requestAnimationFrame(() => inputRef.current?.focus())
  }, [])

  // New chat from either the rail or the mobile header — reset the thread and
  // clear the composer's local draft / mode / attachments in one place.
  const handleNewChat = useCallback(() => {
    onNewChat?.()
    setValue('')
    setMode('auto')
    setFiles([])
  }, [onNewChat])

  // Global kill switch ("Stop all"): interrupts EVERY live Neo run this
  // single-tenant daemon is driving — the escape hatch for a slow model or a
  // storm of parallel / respawning Neos. Distinct from the composer's per-run
  // stop, which halts only the run this thread is watching. Also dismisses the
  // watched task so the surface settles immediately.
  const handleStopAll = useCallback(async () => {
    try {
      const { halted } = await haltAll()
      toast.success(
        halted > 0
          ? `Stopped ${halted} running task${halted === 1 ? '' : 's'}`
          : 'Nothing was running',
      )
    } catch {
      toast.error('Could not stop tasks. Try again.')
    } finally {
      dismissTask()
    }
  }, [dismissTask])

  // "Neo's Computer" (the right work pane) is reserved for live agent ACTIONS —
  // web search, tool / pipeline steps, an Agent Swarm, generated media. A plain
  // chat reply (only the seeded narration, no tools) never opens it; it just
  // streams the answer in the full conversation rail.
  const meaningfulWork =
    !!task &&
    (task.searches.length > 0 ||
      task.media.length > 0 ||
      task.artifacts.length > 0 ||
      !!task.swarm ||
      (task.todos?.length ?? 0) > 0 ||
      task.steps.some((s) => s.kind !== 'narration') ||
      task.surfaces.some((s) => s.kind !== 'narration'))
  // The live, in-flight assistant turn (streaming thoughts + the answer being
  // typed) renders in the conversation rail whenever a run is live. The thought
  // stream is shown in the rail UNLESS "Neo's Computer" is open and already
  // showing it (avoids a desktop double); on the chat path or a phone (computer
  // collapsed) the rail is the single home for Neo's thoughts.
  const showLive = !gated && live
  const railThinking = meaningfulWork && computerOpen ? undefined : task?.thinking

  // Media in the thread (ChatGPT-style): generated images/video render inline
  // in the conversation rail — a reserved skeleton frame per in-flight
  // generation (a running `media` tool.step), each swapped for the real media
  // as its tool.media lands. Driven by task state so it also survives settle
  // and thread reopen (the trace rebuild restores task.media).
  const threadMedia = task?.media ?? []
  const mediaPending = live
    ? (task?.steps.filter((s) => s.kind === 'media' && s.running).length ?? 0)
    : 0

  // Auto-open the work pane on the rising edge of real work — but only on a
  // wide viewport, where the two-pane split fits. On a phone it would bury the
  // conversation, so there it stays collapsed behind the status chip until the
  // user taps to peek. Manual collapses are respected (no reopen until the next
  // task brings fresh work).
  useEffect(() => {
    if (
      !embedded &&
      meaningfulWork &&
      !prevWorkRef.current &&
      typeof window !== 'undefined' &&
      window.matchMedia('(min-width: 1024px)').matches
    ) {
      setComputerOpen(true)
    }
    prevWorkRef.current = meaningfulWork
  }, [meaningfulWork, embedded])

  // Chronology: while a run is live, its trace/searches stack under the latest
  // user bubble. Once the closing answer lands (it arrives as the last thread
  // message), the work renders just above it so the answer stays the bottom of
  // the thread.
  const lastMessage = messages.length > 0 ? messages[messages.length - 1] : null
  const lastIsTaskAnswer =
    !embedded &&
    !!task &&
    task.done &&
    !!lastMessage &&
    lastMessage.role === 'assistant' &&
    (lastMessage.intentId ?? '') === task.intentId
  const thread = lastIsTaskAnswer ? messages.slice(0, -1) : messages

  // Screen-reader announcement: phase transitions, then the final answer once
  // the task settles. A pending consent gate is assertive (it needs action);
  // everything else is polite.
  const announce = gated
    ? 'Approval needed.'
    : phase === 'thinking'
      ? 'Thinking.'
      : phase === 'working'
        ? 'Working.'
        : task?.done
          ? task.answer
            ? `Ready. ${task.answer}`
            : 'Ready.'
          : ''

  return (
    <div className="bg-background relative flex h-full w-full overflow-hidden">
      <div className="sr-only" aria-live={gated ? 'assertive' : 'polite'} aria-atomic="true">
        {announce}
      </div>

      {/* persistent left sidebar (md+): brand, new task, search, real tasks list */}
      <NeoSidebar
        conversations={conversations}
        activeConversationId={conversationId}
        onNewChat={handleNewChat}
        onSelectConversation={onSelectConversation}
        onOpenHistory={onOpenHistory}
        onOpenTimeline={onOpenTimeline}
        onOpenFiles={onOpenFiles}
        onOpenSelfModel={onOpenSelfModel}
        onOpenSettings={onOpenSettings}
        onOpenWallet={onOpenWallet}
      />

      {/* phone drawer — the same sidebar body behind the header hamburger */}
      <NeoMobileSidebar
        open={mobileSidebarOpen}
        onOpenChange={setMobileSidebarOpen}
        conversations={conversations}
        activeConversationId={conversationId}
        onNewChat={handleNewChat}
        onSelectConversation={onSelectConversation}
        onOpenHistory={onOpenHistory}
        onOpenTimeline={onOpenTimeline}
        onOpenFiles={onOpenFiles}
        onOpenSelfModel={onOpenSelfModel}
        onOpenSettings={onOpenSettings}
        onOpenWallet={onOpenWallet}
      />

      {/* the surface column — header, stage, footnote */}
      <div className="relative flex min-w-0 flex-1 flex-col overflow-hidden">
        {/* faint spec-sheet grid — fades out at the edges, no color/glow */}
        <div
          aria-hidden
          className="matrix-grid pointer-events-none absolute inset-0 opacity-60"
          style={{
            maskImage: 'radial-gradient(circle at 50% 42%, #000 26%, transparent 74%)',
            WebkitMaskImage: 'radial-gradient(circle at 50% 42%, #000 26%, transparent 74%)',
          }}
        />

        {/* top chrome — brand + state; the only persistent affordances. Hidden
            below lg while Neo's Computer is open, since the overlay is a
            full-screen takeover with its own header + close on small screens. */}
        <div
          className={cn(
            'relative z-20 flex items-center justify-between px-4 py-3 sm:px-5 sm:py-4',
            computerOpen && 'max-lg:hidden',
          )}
        >
          {/* brand + menu — shown in the header only on mobile; the rail carries
              these on md+. The hamburger opens the full sidebar drawer. */}
          <div className="flex items-center gap-1.5 md:hidden">
            <button
              type="button"
              onClick={() => setMobileSidebarOpen(true)}
              aria-label="Open menu"
              title="Open menu"
              className="text-muted-foreground hover:bg-muted hover:text-foreground grid size-9 place-items-center rounded-full transition"
            >
              <PanelLeftIcon className="size-[1.15rem]" />
            </button>
            <span className="flex items-center gap-2 text-xs font-bold tracking-[0.16em] uppercase">
              <NeoMark className="size-[1.35rem]" /> Neo
            </span>
          </div>
          <div className="flex items-center gap-1.5">
            <StatePill
              phase={phase}
              hasTask={!!task}
              resuming={resuming}
              connectionRetrying={connectionRetrying}
            />
            {/* Global "Stop all" — appears only while a run is live, right beside
                the state. The composer's stop halts this thread's run; this halts
                every live Neo (runaway / parallel respawns). Tone-only danger
                accent, no border/glow. */}
            {live && (
              <button
                type="button"
                onClick={handleStopAll}
                title="Stop all running tasks"
                className="text-muted-foreground hover:bg-muted hover:text-foreground flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium transition-colors"
              >
                <SquareIcon className="size-3 fill-current" />
                Stop all
              </button>
            )}
            {showComputer && meaningfulWork && (
              <ChromeButton
                onClick={() => setComputerOpen((v) => !v)}
                label={computerOpen ? "Hide Neo's Computer" : "Show Neo's Computer"}
              >
                <Monitor className={cn('size-4', computerOpen && 'text-primary')} />
              </ChromeButton>
            )}
            {/* navigation — header on mobile; the rail carries these on md+ */}
            <div className="flex items-center gap-1.5 md:hidden">
              {onOpenHistory && (
                <ChromeButton onClick={onOpenHistory} label="Chat history">
                  <MessageSquare className="size-4" />
                </ChromeButton>
              )}
              {onNewChat && (
                <ChromeButton onClick={handleNewChat} label="New chat">
                  <Plus className="size-4" />
                </ChromeButton>
              )}
              {onOpenSettings && (
                <ChromeButton onClick={onOpenSettings} label="Settings">
                  <Settings className="size-4" />
                </ChromeButton>
              )}
            </div>
          </div>
        </div>

        {/* stage — idle pill is centered; an active conversation fills all the
          available space as a full-height chat column */}
        <div
          className={cn(
            'relative z-10 flex flex-1 flex-col items-center overflow-hidden px-4 pb-[max(env(safe-area-inset-bottom),0.75rem)] sm:px-5',
            conversation ? '' : 'justify-center',
          )}
        >
          {conversation ? (
            <div className="flex w-full min-w-0 flex-1 gap-4 overflow-hidden lg:gap-5">
              {/* LEFT — the conversation rail (chat thread + composer) */}
              <div className="relative mx-auto flex w-full max-w-3xl min-w-0 flex-1 flex-col overflow-hidden">
                {/* body — the conversation thread */}
                <div
                  ref={bodyRef}
                  onScroll={onBodyScroll}
                  className="flex-1 space-y-6 overflow-x-hidden overflow-y-auto overscroll-contain py-5 sm:py-8"
                >
                  {thread.map((m) =>
                    m.role === 'user' ? (
                      <NeoUserMessage key={m.id} message={m} />
                    ) : (
                      <NeoAssistantMessage
                        key={m.id}
                        message={m}
                        onMediaAction={(instruction) => send(instruction)}
                      />
                    ),
                  )}

                  {pendingGate && (
                    <WalletApproval
                      gate={pendingGate}
                      onApprove={(ans) => answerGate(true, ans)}
                      onDeny={() => answerGate(false)}
                    />
                  )}

                  {showLive && (
                    <NeoLiveTurn
                      thinking={railThinking}
                      streamingAnswer={task?.streamingAnswer}
                      label={phase === 'working' ? 'Working…' : 'Thinking…'}
                      reduce={!!reduce}
                      seed={task?.intentId || 'neo-thinking'}
                    />
                  )}

                  {(threadMedia.length > 0 || mediaPending > 0) && (
                    <div className="flex w-full flex-col gap-2">
                      <NeoMediaGrid
                        media={threadMedia}
                        onAction={(instruction) => {
                          send(instruction)
                        }}
                      />
                      {Array.from({ length: mediaPending }).map((_, i) => (
                        <NeoMediaSkeleton key={`media-skeleton-${i}`} />
                      ))}
                    </div>
                  )}

                  {lastIsTaskAnswer && lastMessage && (
                    <NeoAssistantMessage
                      message={lastMessage}
                      failed={task?.failed}
                      onMediaAction={(instruction) => send(instruction)}
                    />
                  )}

                  {!live && !gated && hasThread && (
                    <div className="flex justify-start">
                      <NeoStatusMark live={false} />
                    </div>
                  )}
                </div>

                {/* jump-to-bottom — appears only when scrolled up from the live tail */}
                <AnimatePresence>
                  {showJump && (
                    <motion.button
                      type="button"
                      onClick={() => scrollToBottom('smooth')}
                      aria-label="Jump to latest"
                      initial={reduce ? false : { opacity: 0, y: 6, scale: 0.9 }}
                      animate={{ opacity: 1, y: 0, scale: 1 }}
                      exit={reduce ? { opacity: 0 } : { opacity: 0, y: 6, scale: 0.9 }}
                      transition={reduce ? { duration: 0 } : { duration: 0.18, ease: EASE }}
                      className="bg-surface-tertiary text-foreground ring-border-medium hover:bg-surface-hover absolute bottom-20 left-1/2 grid size-9 -translate-x-1/2 place-items-center rounded-full shadow-lg ring-1 transition-colors"
                    >
                      <ChevronDown className="size-[1.15rem]" />
                    </motion.button>
                  )}
                </AnimatePresence>

                {/* collapsed "Neo's Computer" status chip — reopens the work pane */}
                {showComputer && meaningfulWork && !computerOpen && (
                  <ComputerChip
                    live={live}
                    failed={!!task?.failed}
                    resuming={resuming}
                    connectionRetrying={connectionRetrying}
                    onOpen={() => setComputerOpen(true)}
                  />
                )}

                {/* composer — docked; always present so the user can reply /
                  interrupt / redirect. Hidden only during a consent gate, where
                  the sole next action is approve / deny. */}
                {!gated && (
                  <div className="shrink-0 pt-2">
                    <NeoComposer
                      value={value}
                      onChange={setValue}
                      onSubmit={submit}
                      mode={mode}
                      onModeChange={setMode}
                      variant="bar"
                      inputRef={cardInputRef}
                      placeholder={live ? 'Add to this, or redirect…' : 'Reply to Neo…'}
                      attachments={files}
                      onAddFiles={addFiles}
                      onRemoveFile={removeFile}
                      uploading={uploading}
                      isRunning={runLive}
                      onStop={dismissTask}
                      voiceActive={voice.active}
                      voiceStateLabel={voiceStateLabel}
                      voiceNotice={voice.notice}
                      voiceDevices={voice.devices}
                      voiceDeviceId={voice.deviceId}
                      onVoiceDeviceChange={voice.selectDevice}
                      onVoiceToggle={voice.toggle}
                      voiceStartLabel={t('voiceStart')}
                      voiceStopLabel={t('voiceStop')}
                      voiceMicrophoneLabel={t('voiceMicrophone')}
                    />
                  </div>
                )}
              </div>

              {/* RIGHT — "Neo's Computer": the live work pane. In-flow column on
                lg+, a full-bleed overlay drawer below it. Openable / closable;
                closing is NOT an abort (the run keeps going). */}
              <AnimatePresence>
                {showComputer && meaningfulWork && computerOpen && task && (
                  <NeoComputer
                    key="neo-computer"
                    task={task}
                    phase={phase}
                    reduce={!!reduce}
                    showMedia={!lastIsTaskAnswer}
                    onRespond={respondAsk}
                    onMediaAction={(instruction) => send(instruction)}
                    onClose={() => setComputerOpen(false)}
                    className="fixed inset-0 z-40 rounded-none lg:static lg:z-auto lg:my-6 lg:w-[44%] lg:rounded-2xl xl:w-[46%]"
                  />
                )}
              </AnimatePresence>
            </div>
          ) : (
            <div className="flex w-full max-w-[760px] flex-col items-center">
              {/* headline — personalized time-based greeting */}
              <div className="mb-8 flex w-full flex-col items-center gap-5 px-2 text-center sm:px-8">
                <NeoMark className="size-9" />
                <h1 className="text-foreground text-[1.75rem] leading-[1.15] font-normal tracking-[-0.015em] sm:text-[2.05rem]">
                  {(() => {
                    const hour = new Date().getHours()
                    const greeting =
                      hour < 12 ? 'Good morning' : hour < 18 ? 'Good afternoon' : 'Good evening'
                    return userName ? `${greeting}, ${userName}` : greeting
                  })()}
                </h1>
                <p className="text-muted-foreground/70 text-sm">What would you like to work on?</p>
              </div>
              <div className="bg-card ring-border-medium w-full rounded-[1.75rem] ring-1">
                <NeoComposer
                  value={value}
                  onChange={setValue}
                  onSubmit={submit}
                  mode={mode}
                  onModeChange={setMode}
                  variant="pill"
                  inputRef={inputRef}
                  attachments={files}
                  onAddFiles={addFiles}
                  onRemoveFile={removeFile}
                  uploading={uploading}
                  voiceActive={voice.active}
                  voiceStateLabel={voiceStateLabel}
                  voiceNotice={voice.notice}
                  voiceDevices={voice.devices}
                  voiceDeviceId={voice.deviceId}
                  onVoiceDeviceChange={voice.selectDevice}
                  onVoiceToggle={voice.toggle}
                  voiceStartLabel={t('voiceStart')}
                  voiceStopLabel={t('voiceStop')}
                  voiceMicrophoneLabel={t('voiceMicrophone')}
                />
              </div>

              {/* quick actions — the ONE row under the composer. Each arms a
                REAL composer tool / prompt, then focuses the input for review.
                Tone-only chips, no borders. */}
              <div className="mt-5 flex w-full items-center justify-center gap-2">
                {NEO_QUICK.map((q) => {
                  const Icon = q.icon
                  return (
                    <button
                      key={q.label}
                      type="button"
                      onClick={() => applyQuickAction(q)}
                      className="bg-card text-muted-foreground hover:bg-surface-hover hover:text-foreground flex items-center gap-2 rounded-full px-3 py-1.5 text-[0.8125rem] font-medium whitespace-nowrap transition-colors"
                    >
                      <Icon className="size-[0.9rem] opacity-70" />
                      {q.label}
                    </button>
                  )
                })}
              </div>
            </div>
          )}
        </div>

        {/* footnote — the thesis, shown only at idle rest */}
        <div
          className={cn(
            'relative z-10 px-4 pb-4 text-center font-mono text-[0.7rem] transition-opacity duration-300',
            conversation ? 'pointer-events-none opacity-0' : 'text-muted-foreground/70 opacity-100',
          )}
        ></div>
      </div>
    </div>
  )
}

/** ComputerChip — the collapsed handle for "Neo's Computer". Shown above the
 *  composer whenever there is live work but the pane is closed; tapping it
 *  reopens the pane. A compact echo of the panel header (icon + title + a live
 *  status dot), so the user always knows Neo is busy and how to peek. */
function ComputerChip({
  live,
  failed,
  resuming,
  connectionRetrying,
  onOpen,
}: {
  live: boolean
  failed: boolean
  /** F2 — F1 reattach is mid-replay. */
  resuming?: boolean
  /** F2 — non-terminal stream drop is being retried. */
  connectionRetrying?: boolean
  onOpen: () => void
}) {
  const dotStyle = failed
    ? { background: 'oklch(0.62 0.2 25)' }
    : !live && !resuming && !connectionRetrying
      ? { background: DONE_COLOR }
      : undefined
  // F2 — precedence: connection-retrying (most acute) → resuming → live →
  // failed → settled. Plain language only (ux_truth).
  const status = connectionRetrying
    ? 'Connection lost — retrying…'
    : resuming
      ? 'Reconnecting to your task…'
      : live
        ? 'working…'
        : failed
          ? 'stopped'
          : 'view workspace'
  const pulse = live || resuming || connectionRetrying
  return (
    <button
      type="button"
      onClick={onOpen}
      className="bg-card hover:bg-muted/60 mb-2 flex w-full items-center gap-3 rounded-2xl px-3.5 py-2.5 text-left transition-colors"
    >
      <span className="bg-primary/15 text-primary grid size-8 shrink-0 place-items-center rounded-[0.7rem]">
        <Monitor className="size-[1.05rem]" />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-foreground text-[0.85rem] leading-tight font-bold">
          Neo&apos;s Computer
        </p>
        <p className="text-muted-foreground mt-0.5 flex items-center gap-1.5 font-mono text-[0.68rem]">
          <span
            className={cn('size-1.5 rounded-full', pulse && 'bg-primary animate-pulse')}
            style={dotStyle}
          />
          {status}
        </p>
      </div>
      <ChevronDown className="text-muted-foreground/60 size-4 shrink-0 -rotate-90" />
    </button>
  )
}
