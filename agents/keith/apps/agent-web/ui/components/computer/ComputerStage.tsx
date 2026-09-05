'use client'

import { useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent, PointerEvent, ReactNode } from 'react'
import { Monitor, Refresh, Stop, Warning, X } from '@/components/icons'
import type { Command, CommandResult } from '@/lib/keith'

export type ComputerOwner = 'keith_control' | 'user_control' | 'paused'
export type ComputerConnection = 'negotiating' | 'connected' | 'reconnecting' | 'closed'

export interface ComputerScreenProjection {
  id: string
  computer_session_id: string
  profile_id: string
  lifecycle: string
  connection: ComputerConnection
  quality: 'low' | 'balanced' | 'high'
  owner: ComputerOwner
  lease_revision: number
  frame_sequence: number
  viewport: { width: number; height: number }
  stream_path?: string | null
  active_action?: string | null
  intended_action?: string | null
  recording: boolean
  safe_error?: string | null
}

export interface ComputerStageProps {
  screen: ComputerScreenProjection | null
  csrf: string
  fallback: ReactNode
  onCommand: (command: Command) => Promise<CommandResult | null>
}

type ScaleMode = 'fit' | 'actual' | 'fill'

export function ComputerStage({ screen, csrf, fallback, onCommand }: ComputerStageProps) {
  const viewport = useRef<HTMLDivElement>(null)
  const image = useRef<HTMLImageElement>(null)
  const pointerStart = useRef<{ x: number; y: number; pointerId: number; kind: string } | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [scale, setScale] = useState<ScaleMode>('fit')
  const [streamFailed, setStreamFailed] = useState(false)
  const [notice, setNotice] = useState<string | null>(null)
  const safeStream = useMemo(() => safeScreenPath(screen?.stream_path), [screen?.stream_path])
  const userControls = screen?.owner === 'user_control'
  const inputReady = userControls && screen?.connection === 'connected' && Boolean(safeStream)

  useEffect(() => {
    setStreamFailed(false)
    setNotice(null)
  }, [safeStream, screen?.frame_sequence])

  if (!screen) return <>{fallback}</>

  const mutate = async (action: string, parameters: Record<string, unknown> = {}) => {
    if (busy) return
    setBusy(action)
    setNotice(null)
    try {
      const result = await onCommand({
        command: 'computer',
        parameters: {
          action,
          screen_id: screen.id,
          computer_session_id: screen.computer_session_id,
          expected_revision: screen.lease_revision,
          ...parameters,
        },
      })
      if (!result) setNotice('Keith could not reach the computer control service.')
      else if (result.result.status === 'rejected') {
        setNotice(result.result.payload.error?.safe_message || 'The computer action was rejected.')
      }
    } catch {
      setNotice('The computer action failed safely. Refresh the screen state and try again.')
    } finally {
      setBusy(null)
    }
  }

  const pointerPoint = (event: PointerEvent<HTMLDivElement>) => {
    if (!inputReady || busy) return
    const bounds = image.current?.getBoundingClientRect()
    if (!bounds || !bounds.width || !bounds.height) return
    if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) return
    return {
      x: Math.round(((event.clientX - bounds.left) / bounds.width) * screen.viewport.width),
      y: Math.round(((event.clientY - bounds.top) / bounds.height) * screen.viewport.height),
    }
  }

  const beginPointer = (event: PointerEvent<HTMLDivElement>) => {
    const point = pointerPoint(event)
    if (!point) return
    event.currentTarget.focus()
    event.currentTarget.setPointerCapture(event.pointerId)
    pointerStart.current = { ...point, pointerId: event.pointerId, kind: event.pointerType }
  }

  const finishPointer = (event: PointerEvent<HTMLDivElement>) => {
    const start = pointerStart.current
    pointerStart.current = null
    if (!start || start.pointerId !== event.pointerId) return
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId)
    }
    const point = pointerPoint(event)
    if (!point) return
    const moved = Math.abs(point.x - start.x) + Math.abs(point.y - start.y) > 6
    void mutate('input', {
      input: moved ? 'drag' : start.kind === 'touch' ? 'touch' : 'pointer',
      ...(moved ? { from: { x: start.x, y: start.y }, to: point } : { point }),
      frame_sequence: screen.frame_sequence,
    })
  }

  const sendKey = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!inputReady || busy || event.nativeEvent.isComposing) return
    if (['Tab', 'Escape'].includes(event.key) && !event.ctrlKey && !event.metaKey) return
    event.preventDefault()
    void mutate('input', {
      input: 'keyboard',
      key: event.key,
      code: event.code,
      alt: event.altKey,
      control: event.ctrlKey,
      meta: event.metaKey,
      shift: event.shiftKey,
      frame_sequence: screen.frame_sequence,
    })
  }

  const pasteClipboard = async () => {
    if (!inputReady || busy) return
    try {
      const text = await navigator.clipboard.readText()
      await mutate('clipboard_write', { text, frame_sequence: screen.frame_sequence })
    } catch {
      setNotice('Clipboard access was not granted by this browser.')
    }
  }

  const upload = async (file: File | undefined) => {
    if (!file || !inputReady || busy) return
    setBusy('file_transfer')
    setNotice(null)
    try {
      const body = new FormData()
      body.set('file', file)
      body.set('profile_id', screen.profile_id)
      body.set('expected_revision', String(screen.lease_revision))
      body.set('frame_sequence', String(screen.frame_sequence))
      const response = await fetch(`/api/computers/${encodeURIComponent(screen.id)}/files`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'x-keith-csrf': csrf },
        body,
      })
      if (!response.ok) setNotice(await safeUploadError(response))
    } catch {
      setNotice('The file transfer failed safely.')
    } finally {
      setBusy(null)
    }
  }

  const ownerLabel = screen.owner === 'keith_control'
    ? 'Keith has control'
    : screen.owner === 'user_control'
      ? 'You have control'
      : 'Input paused'

  return (
    <section className="computer-stage" aria-label="Live Keith computer" data-owner={screen.owner}>
      <div className="computer-statusbar">
        <div className="computer-status-copy">
          <span className={`computer-connection ${screen.connection}`} aria-hidden="true" />
          <span><strong>{ownerLabel}</strong><small>{friendly(screen.connection)} · {friendly(screen.quality)} quality · {friendly(screen.lifecycle)}</small></span>
        </div>
        <div className="computer-status-actions">
          {screen.recording ? <span className="recording-pill"><span /> Recording</span> : null}
          <button className="text-button" disabled={busy === 'reconnect_stream'} onClick={() => void mutate('reconnect_stream')}><Refresh size={14} /> Reconnect</button>
        </div>
      </div>

      <div
        ref={viewport}
        className={`computer-viewport scale-${scale}`}
        tabIndex={inputReady ? 0 : -1}
        aria-label={inputReady ? 'Computer screen. Keyboard and pointer input are active.' : `Computer screen. ${ownerLabel}.`}
        onPointerDown={beginPointer}
        onPointerUp={finishPointer}
        onPointerCancel={() => { pointerStart.current = null }}
        onKeyDown={sendKey}
      >
        {safeStream && !streamFailed ? (
          <img
            ref={image}
            src={safeStream}
            alt="Live screen from Keith's isolated computer"
            draggable={false}
            onError={() => setStreamFailed(true)}
          />
        ) : (
          <div className="computer-stream-empty" role="status">
            {screen.connection === 'reconnecting' || streamFailed ? <Refresh size={25} /> : <Monitor size={25} />}
            <strong>{streamFailed ? 'Screen connection interrupted' : 'Waiting for the live screen'}</strong>
            <p>{streamFailed ? 'Keith stopped input and is reconnecting through the authenticated stream.' : 'The computer remains isolated while the screen is negotiated.'}</p>
          </div>
        )}
        <div className="computer-action-overlay" aria-live="polite">
          {screen.active_action ? <p><span>Now</span>{screen.active_action}</p> : null}
          {screen.intended_action ? <p><span>Next</span>{screen.intended_action}</p> : null}
        </div>
      </div>

      {screen.safe_error || notice ? <div className="computer-safe-error" role="alert"><Warning size={15} /><span>{notice || screen.safe_error}</span>{notice ? <button aria-label="Dismiss computer error" onClick={() => setNotice(null)}><X size={14} /></button> : null}</div> : null}

      <div className="computer-controls" aria-label="Computer controls">
        <div className="computer-owner-controls">
          {screen.owner === 'user_control' ? (
            <button className="primary-button" disabled={Boolean(busy)} onClick={() => void mutate('grant_keith_control')}>Return control to Keith</button>
          ) : (
            <button className="primary-button" disabled={Boolean(busy)} onClick={() => void mutate('take_user_control')}>Take control</button>
          )}
          <button className="secondary-button" disabled={Boolean(busy) || screen.owner === 'paused'} onClick={() => void mutate('pause_control')}><Stop size={13} /> Pause input</button>
        </div>
        <div className="computer-accessory-controls">
          <select value={scale} onChange={(event) => setScale(event.target.value as ScaleMode)} aria-label="Screen scaling">
            <option value="fit">Fit screen</option>
            <option value="actual">Actual size</option>
            <option value="fill">Fill stage</option>
          </select>
          <button className="secondary-button" disabled={!inputReady || Boolean(busy)} onClick={() => void pasteClipboard()}>Paste clipboard</button>
          <select
            defaultValue=""
            disabled={!inputReady || Boolean(busy)}
            aria-label="Send a computer key"
            onChange={(event) => {
              if (!event.target.value) return
              void mutate('input', { input: 'keyboard', key: event.target.value, code: event.target.value, frame_sequence: screen.frame_sequence })
              event.currentTarget.value = ''
            }}
          >
            <option value="">Send key…</option>
            <option value="Tab">Tab</option>
            <option value="Escape">Escape</option>
            <option value="Enter">Enter</option>
            <option value="Backspace">Backspace</option>
            <option value="Delete">Delete</option>
          </select>
          <label className={`secondary-button ${!inputReady || busy ? 'is-disabled' : ''}`}>
            Send file
            <input type="file" disabled={!inputReady || Boolean(busy)} onChange={(event) => { void upload(event.target.files?.[0]); event.currentTarget.value = '' }} />
          </label>
          <button className="secondary-button" onClick={() => void viewport.current?.requestFullscreen()}>Full screen</button>
        </div>
      </div>
    </section>
  )
}

export function screenProjection(value: unknown): ComputerScreenProjection | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const candidate = value as Record<string, unknown>
  const control = candidate.control
  const viewport = candidate.viewport
  if (!control || typeof control !== 'object' || !viewport || typeof viewport !== 'object') return null
  const lease = control as Record<string, unknown>
  const dimensions = viewport as Record<string, unknown>
  if (
    !safeId(candidate.id)
    || !safeId(candidate.computer_session_id)
    || !safeId(candidate.profile_id)
    || !['negotiating', 'connected', 'reconnecting', 'closed'].includes(String(candidate.connection))
    || !['low', 'balanced', 'high'].includes(String(candidate.quality))
    || !['keith_control', 'user_control', 'paused'].includes(String(lease.owner))
    || !Number.isSafeInteger(lease.revision)
    || !Number.isSafeInteger(candidate.frame_sequence)
    || !Number.isSafeInteger(dimensions.width)
    || !Number.isSafeInteger(dimensions.height)
  ) return null
  return {
    id: candidate.id as string,
    computer_session_id: candidate.computer_session_id as string,
    profile_id: candidate.profile_id as string,
    lifecycle: safeText(candidate.lifecycle, 'unknown'),
    connection: candidate.connection as ComputerConnection,
    quality: candidate.quality as ComputerScreenProjection['quality'],
    owner: lease.owner as ComputerOwner,
    lease_revision: lease.revision as number,
    frame_sequence: candidate.frame_sequence as number,
    viewport: { width: dimensions.width as number, height: dimensions.height as number },
    stream_path: safeScreenPath(candidate.stream_path),
    active_action: optionalSafeText(candidate.active_action),
    intended_action: optionalSafeText(candidate.intended_action),
    recording: candidate.recording === true,
    safe_error: optionalSafeText(candidate.safe_error),
  }
}

function safeScreenPath(value: unknown): string | null {
  if (typeof value !== 'string' || value.length > 2_048) return null
  if (!/^\/api\/computers\/[a-zA-Z0-9_-]{1,128}\/screen$/.test(value)) return null
  return value
}

function safeId(value: unknown): value is string {
  return typeof value === 'string' && /^[a-zA-Z0-9_-]{1,128}$/.test(value)
}

function optionalSafeText(value: unknown): string | null {
  if (typeof value !== 'string' || !value.trim() || value.length > 2_048 || /[\u0000-\u0008\u000b\u000c\u000e-\u001f]/.test(value)) return null
  const normalized = value.toLowerCase()
  return ['authorization: bearer', 'access_token', 'refresh_token', 'api_key', 'password=', 'secret=', 'sk-']
    .some((marker) => normalized.includes(marker)) ? null : value
}

function safeText(value: unknown, fallback: string): string {
  return optionalSafeText(value) || fallback
}

function friendly(value: string): string {
  return value.replaceAll('_', ' ').replace(/\b\w/g, (letter) => letter.toUpperCase())
}

async function safeUploadError(response: Response): Promise<string> {
  try {
    const payload = await response.json() as { error?: { safe_message?: unknown } }
    if (typeof payload.error?.safe_message === 'string') return payload.error.safe_message.slice(0, 512)
  } catch {}
  return `The file transfer was rejected (${response.status}).`
}
