'use client'

/**
 * NeoDesktop — the disposable desktop panel (DOJO wave 3, req 4.1/4.2/4.3).
 *
 * Renders the dojo desktop's lifecycle inside Neo's Computer: provisioning is
 * presented as the computer TURNING ON (a calm boot screen with cycling status
 * lines — never a spinner-error), the attached states show a polled live view
 * of the real desktop, shipping shows the work going home, and destroyed is an
 * honest "off" screen that names why (and never hides a failed ship).
 *
 * Live-view transport: the panel polls one authed JPEG frame at a time from
 * `/dojo/frame` while visible — plain request/response with an in-flight guard
 * (the next poll is scheduled only after the last one settles), no held
 * stream, nothing to keep alive. Frames are passive on the server side too:
 * watching never keeps the sandbox running.
 *
 * Takeover (req 4.2): "Take control" moves the server-owned control lease to
 * the human; while held, the frame area captures pointer/keyboard input and
 * passes it through to the desktop, and the agent's own actions are refused
 * server-side. "Hand back" is the explicit handback event — the agent then
 * re-observes before acting (enforced server-side).
 *
 * House rules: separation by background TONE only (no border strokes), no
 * emojis / gradients / glow; every string localized (all 5 locales).
 */
import { useEffect, useRef, useState } from 'react'
import { useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'
import type { NeoDojo } from '@/hooks/api/useChat'
import {
  loadDojoFrame,
  getDojoSession,
  dojoBoot,
  dojoShutdown,
  dojoTakeover,
  dojoHandback,
  sendDojoInput,
  type DojoInputRequest,
  type DojoSession,
} from '@/lib/api/dojo'
import Loader from '@/components/ui/box-loader'

const FRAME_POLL_MS = 3000
/** Faster cadence while the human drives — their own clicks need echo. */
const FRAME_POLL_TAKEOVER_MS = 1200
const BOOT_LINE_MS = 2600
const SESSION_POLL_MS = 4000

const LIVE_STATES = new Set(['provisioning', 'ready', 'active', 'takeover', 'shipping'])

function dojoFromSession(s: DojoSession, conv: string): NeoDojo {
  return {
    state: (s.state as NeoDojo['state']) || 'off',
    sessionId: s.id,
    conversationId: conv,
    reason: s.reason,
  }
}

/**
 * The desktop's effective state: SERVER TRUTH first, events second. The user
 * can power the desktop on and off with no run streaming events, and a reaped
 * sandbox must never render as live — so while the state is non-terminal this
 * hook polls GET /dojo/session (sequential, visibility-paused) and the poll
 * result is authoritative; the folded dojo.* events seed it instantly, carry
 * the rich terminal detail (reason, ship_error), and trigger a fresh poll on
 * every change. `apply` feeds a route response (boot / takeover / handback)
 * straight in so user actions render without waiting a poll cycle.
 */
function useDojoSession(conv: string | undefined, eventDojo?: NeoDojo) {
  const [server, setServer] = useState<{ loaded: boolean; dojo: NeoDojo | null }>({
    loaded: false,
    dojo: null,
  })

  const effective: NeoDojo | null = server.loaded
    ? (server.dojo ??
      (eventDojo && (eventDojo.state === 'destroyed' || eventDojo.state === 'failed')
        ? eventDojo
        : null))
    : (eventDojo ?? null)

  const live = !server.loaded || (effective !== null && LIVE_STATES.has(effective.state))

  useEffect(() => {
    if (!conv || !live) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | null = null
    const tick = async () => {
      if (stopped) return
      if (typeof document === 'undefined' || document.visibilityState !== 'hidden') {
        const s = await getDojoSession(conv)
        if (stopped) return
        setServer({ loaded: true, dojo: s ? dojoFromSession(s, conv) : null })
      }
      timer = setTimeout(() => void tick(), SESSION_POLL_MS)
    }
    void tick()
    return () => {
      stopped = true
      if (timer) clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- re-arm on event
    // transitions so a pushed dojo.* event converges immediately.
  }, [conv, live, eventDojo?.state])

  const apply = (s: DojoSession | null) => {
    if (conv) setServer({ loaded: true, dojo: s ? dojoFromSession(s, conv) : null })
  }
  return { effective, apply }
}

/** Non-printable keys mapped to bytebotd key names (chords ride type_keys —
 *  the wave-2 appliance contract). */
const KEYMAP: Record<string, string> = {
  Enter: 'enter',
  Backspace: 'backspace',
  Tab: 'tab',
  Escape: 'escape',
  ArrowUp: 'up',
  ArrowDown: 'down',
  ArrowLeft: 'left',
  ArrowRight: 'right',
  Delete: 'delete',
  Home: 'home',
  End: 'end',
  PageUp: 'pageup',
  PageDown: 'pagedown',
}

/** Poll the live-view frame while the desktop is attached and the tab is
 *  visible. Sequential (await-then-schedule) so requests never overlap; every
 *  object URL is revoked when replaced or on unmount. */
function useDojoFrame(conversationId: string | undefined, live: boolean, fast: boolean) {
  const [frame, setFrame] = useState<string | null>(null)
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null)
  useEffect(() => {
    if (!conversationId || !live) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | null = null
    let current: string | null = null
    const gap = fast ? FRAME_POLL_TAKEOVER_MS : FRAME_POLL_MS
    const tick = async () => {
      if (stopped) return
      if (typeof document === 'undefined' || document.visibilityState !== 'hidden') {
        const url = await loadDojoFrame(conversationId)
        if (stopped) {
          if (url) URL.revokeObjectURL(url)
          return
        }
        if (url) {
          if (current) URL.revokeObjectURL(current)
          current = url
          setFrame(url)
          setUpdatedAt(new Date())
        }
      }
      timer = setTimeout(() => void tick(), gap)
    }
    void tick()
    return () => {
      stopped = true
      if (timer) clearTimeout(timer)
      if (current) URL.revokeObjectURL(current)
      setFrame(null)
      setUpdatedAt(null)
    }
  }, [conversationId, live, fast])
  return { frame, updatedAt }
}

/** The cycling boot line index (the computer turning on). */
function useBootLine(active: boolean, count: number) {
  const [line, setLine] = useState(0)
  useEffect(() => {
    if (!active) return
    setLine(0)
    const id = setInterval(() => setLine((n) => (n + 1) % count), BOOT_LINE_MS)
    return () => clearInterval(id)
  }, [active, count])
  return line
}

/** Map a client point to desktop pixels through the object-contain letterbox
 *  of the rendered frame. Pure — exported for the wave-3 tests. Returns null
 *  outside the visible desktop area. */
export function mapToDesktop(
  clientX: number,
  clientY: number,
  rect: { left: number; top: number; width: number; height: number },
  naturalWidth: number,
  naturalHeight: number,
): { x: number; y: number } | null {
  if (!naturalWidth || !naturalHeight || rect.width <= 0 || rect.height <= 0) return null
  const scale = Math.min(rect.width / naturalWidth, rect.height / naturalHeight)
  if (scale <= 0) return null
  const w = naturalWidth * scale
  const h = naturalHeight * scale
  const ox = rect.left + (rect.width - w) / 2
  const oy = rect.top + (rect.height - h) / 2
  const x = (clientX - ox) / scale
  const y = (clientY - oy) / scale
  if (x < 0 || y < 0 || x > naturalWidth || y > naturalHeight) return null
  return { x: Math.round(x), y: Math.round(y) }
}

function offCopyKey(dojo: NeoDojo): string {
  if (dojo.state === 'failed') return 'offFailed'
  if (dojo.state === 'off') return 'offNever'
  switch (dojo.reason) {
    case 'idle_timeout':
      return 'offIdle'
    case 'max_lifetime':
      return 'offLifetime'
    case 'discard':
      return 'offDiscard'
    case 'provision_failed':
      return 'offFailed'
    case 'sandbox_dead':
      return 'offCrashed'
    default:
      return 'offGeneric'
  }
}

export function NeoDesktop({
  dojo,
  onBoot,
  onShutdown,
  onTakeover,
  onHandback,
  onInput,
  takeoverBusy,
  powerBusy,
}: {
  dojo: NeoDojo
  /** Turn the computer ON — the user's power button (never agent-dependent).
   *  Absent hides the control. */
  onBoot?: () => void
  /** Turn the computer OFF (ship-first teardown). Absent hides the control. */
  onShutdown?: () => void
  /** Take the control lease (req 4.2). Absent hides the control. */
  onTakeover?: () => void
  /** Hand the controls back to Neo (req 4.3). Absent hides the control. */
  onHandback?: () => void
  /** Pass one human input action through while the lease is held. */
  onInput?: (request: DojoInputRequest) => void
  /** True while a lease change is in flight (debounces the button). */
  takeoverBusy?: boolean
  /** True while a power change is in flight (debounces the buttons). */
  powerBusy?: boolean
}) {
  const t = useTranslations('dojoDesktop')
  const booting = dojo.state === 'provisioning'
  const takeover = dojo.state === 'takeover'
  const attached = dojo.state === 'ready' || dojo.state === 'active' || takeover
  const { frame, updatedAt } = useDojoFrame(dojo.conversationId, attached, takeover)
  const bootLine = useBootLine(booting, 3)
  const imgRef = useRef<HTMLImageElement>(null)

  /** Map a pointer event on the overlay to desktop pixels through the
   *  object-contain letterbox of the current frame. */
  const desktopCoords = (e: React.MouseEvent): { x: number; y: number } | null => {
    const img = imgRef.current
    if (!img) return null
    return mapToDesktop(
      e.clientX,
      e.clientY,
      img.getBoundingClientRect(),
      img.naturalWidth,
      img.naturalHeight,
    )
  }

  const clickThrough = (e: React.MouseEvent, button: 'left' | 'right') => {
    if (!onInput) return
    const coordinates = desktopCoords(e)
    if (!coordinates) return
    onInput({ action: 'click_mouse', coordinates, button, clickCount: 1 })
  }

  const keyThrough = (e: React.KeyboardEvent) => {
    if (!onInput) return
    const special = KEYMAP[e.key]
    const mods: string[] = []
    if (e.ctrlKey) mods.push('ctrl')
    if (e.altKey) mods.push('alt')
    if (e.metaKey) mods.push('meta')
    if (special || mods.length > 0) {
      const key = special || (e.key.length === 1 ? e.key.toLowerCase() : null)
      if (!key) return
      if (e.shiftKey && special) mods.push('shift')
      e.preventDefault()
      onInput({ action: 'type_keys', keys: [...mods, key] })
      return
    }
    if (e.key.length === 1) {
      e.preventDefault()
      onInput({ action: 'type_text', text: e.key })
    }
  }

  const stateLabel = booting
    ? t('stateBooting')
    : takeover
      ? t('stateTakeover')
      : dojo.state === 'active'
        ? t('stateAgent')
        : dojo.state === 'ready'
          ? t('stateReady')
          : dojo.state === 'shipping'
            ? t('stateShipping')
            : t('stateOff')

  return (
    <div className="overflow-hidden rounded-xl">
      {/* window chrome — tone-only separation from the desktop body */}
      <div className="bg-surface-primary-contrast flex items-center gap-2 px-3 py-2">
        <span className="flex items-center gap-1" aria-hidden>
          <span className="bg-foreground/15 size-2 rounded-full" />
          <span className="bg-foreground/15 size-2 rounded-full" />
          <span className="bg-foreground/15 size-2 rounded-full" />
        </span>
        <span className="text-foreground/80 flex min-w-0 items-center gap-1.5 truncate font-mono text-[0.7rem]">
          <span
            className={cn(
              'size-1.5 shrink-0 rounded-full',
              (booting || dojo.state === 'shipping') && 'bg-primary animate-pulse',
              attached && 'bg-primary',
              (dojo.state === 'destroyed' || dojo.state === 'failed' || dojo.state === 'off') &&
                'bg-foreground/25',
            )}
          />
          {stateLabel}
        </span>
        <span className="min-w-0 flex-1" />
        {attached && updatedAt ? (
          <span className="text-muted-foreground shrink-0 font-mono text-[0.65rem]">
            {t('updated', { time: updatedAt.toLocaleTimeString() })}
          </span>
        ) : null}
        {(attached || booting) && onShutdown ? (
          <button
            type="button"
            onClick={onShutdown}
            disabled={powerBusy}
            className="text-muted-foreground hover:bg-muted hover:text-foreground shrink-0 rounded-lg px-2 py-1 text-[0.7rem] font-medium transition-colors disabled:opacity-50"
          >
            {t('turnOff')}
          </button>
        ) : null}
        {attached && onTakeover && !takeover ? (
          <button
            type="button"
            onClick={onTakeover}
            disabled={takeoverBusy}
            className="text-primary hover:bg-primary/10 shrink-0 rounded-lg px-2 py-1 text-[0.7rem] font-medium transition-colors disabled:opacity-50"
          >
            {t('takeControl')}
          </button>
        ) : null}
        {onHandback && takeover ? (
          <button
            type="button"
            onClick={onHandback}
            disabled={takeoverBusy}
            className="bg-primary/15 text-primary hover:bg-primary/25 shrink-0 rounded-lg px-2 py-1 text-[0.7rem] font-medium transition-colors disabled:opacity-50"
          >
            {t('handBack')}
          </button>
        ) : null}
      </div>

      {/* the desktop screen */}
      <div className="bg-surface-primary-alt relative aspect-[4/3] w-full">
        {booting ? (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-4 px-6 text-center">
            <Loader />
            <div>
              <p className="text-foreground text-sm font-medium">{t('bootTitle')}</p>
              <p className="text-muted-foreground mt-1 text-xs">{t(`bootLine${bootLine + 1}`)}</p>
            </div>
          </div>
        ) : attached ? (
          frame ? (
            <>
              {/* eslint-disable-next-line @next/next/no-img-element -- authed
                  blob object URL; next/image cannot carry the bearer */}
              <img
                ref={imgRef}
                src={frame}
                alt={t('frameAlt')}
                className="absolute inset-0 h-full w-full object-contain"
                draggable={false}
              />
              {takeover && onInput ? (
                <div
                  data-testid="dojo-input-overlay"
                  role="application"
                  aria-label={t('stateTakeover')}
                  tabIndex={0}
                  className="absolute inset-0 cursor-crosshair outline-none"
                  onClick={(e) => {
                    e.currentTarget.focus()
                    clickThrough(e, 'left')
                  }}
                  onContextMenu={(e) => {
                    e.preventDefault()
                    clickThrough(e, 'right')
                  }}
                  onWheel={(e) => {
                    const coordinates = desktopCoords(e)
                    if (!coordinates) return
                    onInput({
                      action: 'scroll',
                      coordinates,
                      direction: e.deltaY > 0 ? 'down' : 'up',
                      scrollCount: 3,
                    })
                  }}
                  onKeyDown={keyThrough}
                />
              ) : null}
            </>
          ) : (
            <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 px-6 text-center">
              <Loader />
              <p className="text-muted-foreground text-xs">{t('waitingFrame')}</p>
            </div>
          )
        ) : dojo.state === 'shipping' ? (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 px-6 text-center">
            <Loader />
            <p className="text-foreground text-sm font-medium">{t('shippingTitle')}</p>
            <p className="text-muted-foreground text-xs">{t('shippingBody')}</p>
          </div>
        ) : (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 px-6 text-center">
            <p className="text-foreground text-sm font-medium">{t('offTitle')}</p>
            <p className="text-muted-foreground max-w-sm text-xs [overflow-wrap:anywhere]">
              {t(offCopyKey(dojo))}
            </p>
            {dojo.shipError ? (
              <p className="text-muted-foreground max-w-sm font-mono text-[0.7rem] [overflow-wrap:anywhere]">
                {t('shipWarning', { error: dojo.shipError })}
              </p>
            ) : null}
            {dojo.state === 'failed' && dojo.reason ? (
              <p className="text-muted-foreground/70 max-w-sm font-mono text-[0.65rem] [overflow-wrap:anywhere]">
                {dojo.reason}
              </p>
            ) : null}
            {onBoot ? (
              <>
                <button
                  type="button"
                  onClick={onBoot}
                  disabled={powerBusy}
                  className="bg-primary/15 text-primary hover:bg-primary/25 mt-2 rounded-lg px-3.5 py-1.5 text-xs font-medium transition-colors disabled:opacity-50"
                >
                  {t('turnOn')}
                </button>
                <p className="text-muted-foreground/70 max-w-sm text-[0.7rem] [overflow-wrap:anywhere]">
                  {t('offHint')}
                </p>
              </>
            ) : null}
          </div>
        )}
      </div>
    </div>
  )
}

/** The stateful desktop screen: the user's computer for this conversation.
 *  Owns the POWER (turn on / turn off — never dependent on the agent), the
 *  lease calls (take control / hand back), and the input passthrough. State is
 *  server-truth via useDojoSession: dojo.* events seed and enrich it, the
 *  session poll keeps it honest with no run streaming, and route responses
 *  apply instantly — the UI never claims a lease or a boot the server did not
 *  grant. */
export function DojoDesktopScreen({
  dojo,
  conversationId,
}: {
  /** The event-folded state (absent when no dojo.* event ever reached this
   *  conversation — the desktop can still be powered on by the user). */
  dojo?: NeoDojo
  /** The conversation the desktop binds to; required for the power button
   *  when no session exists yet. */
  conversationId?: string | null
}) {
  const conv = dojo?.conversationId ?? conversationId ?? undefined
  const { effective, apply } = useDojoSession(conv, dojo)
  const [leaseBusy, setLeaseBusy] = useState(false)
  const [powerBusy, setPowerBusy] = useState(false)
  const [powerError, setPowerError] = useState<string>()

  const shown: NeoDojo = powerError
    ? { state: 'failed', conversationId: conv, reason: powerError }
    : (effective ?? { state: 'off', conversationId: conv })

  const run = async (
    setBusyFlag: (v: boolean) => void,
    fn: (c: string) => Promise<DojoSession | null>,
    reportPowerFailure = false,
  ) => {
    if (!conv) return
    setBusyFlag(true)
    if (reportPowerFailure) setPowerError(undefined)
    try {
      apply(await fn(conv))
    } catch (error) {
      if (reportPowerFailure) {
        setPowerError(error instanceof Error ? error.message : 'The computer could not turn on.')
      }
    } finally {
      setBusyFlag(false)
    }
  }

  return (
    <NeoDesktop
      dojo={shown}
      takeoverBusy={leaseBusy}
      powerBusy={powerBusy}
      onBoot={conv ? () => void run(setPowerBusy, dojoBoot, true) : undefined}
      onShutdown={conv ? () => void run(setPowerBusy, dojoShutdown) : undefined}
      onTakeover={conv ? () => void run(setLeaseBusy, dojoTakeover) : undefined}
      onHandback={conv ? () => void run(setLeaseBusy, dojoHandback) : undefined}
      onInput={(req) => {
        if (conv) void sendDojoInput(conv, req)
      }}
    />
  )
}
