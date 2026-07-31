import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'
import type { TraceEvent } from '@/lib/api/conversations'

/* -------------------------------------------------------------------------- */
/*  DOJO wave 3 — the disposable desktop panel in Neo's Computer (req 4.1)    */
/* -------------------------------------------------------------------------- */
/*  Pins the REAL client code paths: the real trace reducer                   */
/*  (buildTaskFromTrace → foldDojoEvent) and the real NeoComputer/NeoDesktop  */
/*  render. The ONE mocked boundary is the authed frame transport             */
/*  (loadDojoFrame) — the network edge — so the tests assert the live view    */
/*  loads THROUGH the authed loader and the lifecycle states render from the  */
/*  durable dojo.* events (boot = the computer turning on, never an error).   */

const loadDojoFrame = vi.fn(async (conv: string) => `blob:authed-frame:${conv}`)
const getDojoSession = vi.fn(async (_conv: string): Promise<Record<string, unknown> | null> => null)
const dojoBoot = vi.fn(async (conv: string) => ({
  id: 'dojo-abc',
  conversation_id: conv,
  state: 'provisioning',
}))
const dojoShutdown = vi.fn(async (_conv: string) => null)
const dojoTakeover = vi.fn(async (conv: string) => ({
  id: 'dojo-abc',
  conversation_id: conv,
  state: 'takeover',
}))
const dojoHandback = vi.fn(async (conv: string) => ({
  id: 'dojo-abc',
  conversation_id: conv,
  state: 'active',
}))
const sendDojoInput = vi.fn(async (_conv: string, _req: unknown) => true)
vi.mock('@/lib/api/dojo', () => ({
  loadDojoFrame: (conv: string) => loadDojoFrame(conv),
  getDojoSession: (conv: string) => getDojoSession(conv),
  dojoBoot: (conv: string) => dojoBoot(conv),
  dojoShutdown: (conv: string) => dojoShutdown(conv),
  dojoTakeover: (conv: string) => dojoTakeover(conv),
  dojoHandback: (conv: string) => dojoHandback(conv),
  sendDojoInput: (conv: string, req: unknown) => sendDojoInput(conv, req),
}))

/** A GET /dojo/session payload for the poll mock. */
function session(state: string) {
  return { id: 'dojo-abc', conversation_id: 'conv-1', state }
}

import { buildTaskFromTrace, foldDojoEvent } from '@/hooks/api/useChat'
import { NeoComputer } from '@/components/matrix/neo/neo-computer'
import { NeoDesktop, DojoDesktopScreen, mapToDesktop } from '@/components/matrix/neo/neo-desktop'

/** A dojo.* trace frame exactly as the daemon persists it (publishDojo →
 *  traceWorkspaceTypes → handleTrace). */
function dojoEvent(seq: number, type: string, extra?: Record<string, unknown>): TraceEvent {
  return {
    seq,
    ts: `2026-07-24T10:00:0${seq}Z`,
    phase: 'act',
    type,
    fields: {
      intent_id: 'neo_run',
      conversation_id: 'conv-1',
      session_id: 'dojo-abc',
      state: type.replace('dojo.', ''),
      ...extra,
    },
  } as TraceEvent
}

beforeEach(() => {
  loadDojoFrame.mockClear()
  getDojoSession.mockClear()
  getDojoSession.mockResolvedValue(null)
  dojoBoot.mockClear()
  dojoShutdown.mockClear()
  dojoTakeover.mockClear()
  dojoHandback.mockClear()
  sendDojoInput.mockClear()
})

describe('DOJO — reducer folds dojo.* to the last honest state', () => {
  it('follows the lifecycle and maps handback to active', () => {
    const task = buildTaskFromTrace(
      [
        dojoEvent(1, 'dojo.provisioning'),
        dojoEvent(2, 'dojo.ready'),
        dojoEvent(3, 'dojo.active'),
        dojoEvent(4, 'dojo.takeover'),
        dojoEvent(5, 'dojo.handback', { state: 'active' }),
      ],
      'neo_run',
    )
    expect(task.dojo?.state).toBe('active')
    expect(task.dojo?.sessionId).toBe('dojo-abc')
    expect(task.dojo?.conversationId).toBe('conv-1')
  })

  it('keeps the destroy reason and a failed ship honest (req 5.1)', () => {
    const task = buildTaskFromTrace(
      [
        dojoEvent(1, 'dojo.active'),
        dojoEvent(2, 'dojo.shipping', { reason: 'idle_timeout' }),
        dojoEvent(3, 'dojo.destroyed', {
          reason: 'idle_timeout',
          ship_error: 'volume unreachable',
        }),
      ],
      'neo_run',
    )
    expect(task.dojo?.state).toBe('destroyed')
    expect(task.dojo?.reason).toBe('idle_timeout')
    expect(task.dojo?.shipError).toBe('volume unreachable')
  })

  it('ignores non-dojo types', () => {
    expect(foldDojoEvent('tool.step', {})).toBeNull()
  })
})

describe('DOJO — the desktop screen in Neo Computer', () => {
  it('renders provisioning as the computer turning on, without polling frames', () => {
    getDojoSession.mockResolvedValue(session('provisioning'))
    const task = buildTaskFromTrace([dojoEvent(1, 'dojo.provisioning')], 'neo_run')
    renderWithIntl(<NeoComputer task={task} phase="working" reduce showMedia={false} legacyOnly />)
    expect(screen.getByText('The computer is turning on')).toBeTruthy()
    // A boot is a boot — no spinner-error copy, no frame requests yet.
    expect(screen.queryByText(/error|failed/i)).toBeNull()
    expect(loadDojoFrame).not.toHaveBeenCalled()
  })

  it('polls the live view through the authed frame loader once attached', async () => {
    getDojoSession.mockResolvedValue(session('active'))
    const task = buildTaskFromTrace(
      [dojoEvent(1, 'dojo.provisioning'), dojoEvent(2, 'dojo.ready'), dojoEvent(3, 'dojo.active')],
      'neo_run',
    )
    renderWithIntl(<NeoComputer task={task} phase="working" reduce showMedia={false} legacyOnly />)
    await waitFor(() => expect(loadDojoFrame).toHaveBeenCalledWith('conv-1'))
    await waitFor(() => {
      const img = screen.getByAltText("Live view of Neo's desktop") as HTMLImageElement
      expect(img.src).toContain('blob:authed-frame:conv-1')
    })
    expect(screen.getByText('Neo is driving')).toBeTruthy()
  })

  it('renders the honest off state with the ship warning on destroy', () => {
    const task = buildTaskFromTrace(
      [
        dojoEvent(1, 'dojo.active'),
        dojoEvent(2, 'dojo.destroyed', { reason: 'idle_timeout', ship_error: 'volume gone' }),
      ],
      'neo_run',
    )
    renderWithIntl(<NeoComputer task={task} phase="idle" reduce showMedia={false} legacyOnly />)
    expect(screen.getByText('The computer is off')).toBeTruthy()
    expect(screen.getByText(/turned off after sitting idle/i)).toBeTruthy()
    expect(screen.getByText(/volume gone/)).toBeTruthy()
    expect(loadDojoFrame).not.toHaveBeenCalled()
  })

  it('offers Take control while Neo drives and calls the lease API (req 4.2)', async () => {
    getDojoSession.mockResolvedValue(session('active'))
    const dojo = foldDojoEvent('dojo.active', {
      session_id: 'dojo-abc',
      conversation_id: 'conv-1',
    })!
    const { fireEvent } = await import('@testing-library/react')
    renderWithIntl(<DojoDesktopScreen dojo={dojo} />)
    const btn = await screen.findByRole('button', { name: 'Take control' })
    fireEvent.click(btn)
    await waitFor(() => expect(dojoTakeover).toHaveBeenCalledWith('conv-1'))
    // The successful server response is authoritative and applies immediately.
    expect(screen.getByRole('button', { name: 'Hand back to Neo' })).toBeTruthy()
  })

  it('captures input during takeover and passes it through (req 4.2)', async () => {
    getDojoSession.mockResolvedValue(session('takeover'))
    const dojo = foldDojoEvent('dojo.takeover', {
      session_id: 'dojo-abc',
      conversation_id: 'conv-1',
    })!
    const { fireEvent } = await import('@testing-library/react')
    renderWithIntl(<DojoDesktopScreen dojo={dojo} />)
    const overlay = await screen.findByTestId('dojo-input-overlay')
    // Keyboard passthrough (coordinates-free, so jsdom can prove it end to end).
    fireEvent.keyDown(overlay, { key: 'Enter' })
    await waitFor(() =>
      expect(sendDojoInput).toHaveBeenCalledWith('conv-1', {
        action: 'type_keys',
        keys: ['enter'],
      }),
    )
    fireEvent.keyDown(overlay, { key: 'a' })
    await waitFor(() =>
      expect(sendDojoInput).toHaveBeenCalledWith('conv-1', { action: 'type_text', text: 'a' }),
    )
    fireEvent.keyDown(overlay, { key: 'c', ctrlKey: true })
    await waitFor(() =>
      expect(sendDojoInput).toHaveBeenCalledWith('conv-1', {
        action: 'type_keys',
        keys: ['ctrl', 'c'],
      }),
    )
    // Hand back is the explicit event trigger.
    fireEvent.click(screen.getByRole('button', { name: 'Hand back to Neo' }))
    await waitFor(() => expect(dojoHandback).toHaveBeenCalledWith('conv-1'))
  })

  it('lets the USER turn the computer on with no agent involvement (power)', async () => {
    const { fireEvent } = await import('@testing-library/react')
    // No dojo.* event has ever existed — just a conversation. The desktop
    // screen still renders (off) with the power button.
    renderWithIntl(<DojoDesktopScreen conversationId="conv-1" />)
    expect(await screen.findByText('The computer is off')).toBeTruthy()
    expect(screen.getByText(/isn't running right now/i)).toBeTruthy()
    const on = screen.getByRole('button', { name: 'Turn on' })
    // Once booted, the server reports the provisioning session on the poll.
    getDojoSession.mockResolvedValue(session('provisioning'))
    fireEvent.click(on)
    await waitFor(() => expect(dojoBoot).toHaveBeenCalledWith('conv-1'))
    // The boot response applies instantly: the computer is turning on.
    await waitFor(() => expect(screen.getByText('The computer is turning on')).toBeTruthy())
  })

  it('shows the provisioning failure returned by the power route', async () => {
    const { fireEvent } = await import('@testing-library/react')
    dojoBoot.mockRejectedValueOnce(new Error('dojo: provision failed: create sandbox rejected'))
    renderWithIntl(<DojoDesktopScreen conversationId="conv-1" />)

    fireEvent.click(await screen.findByRole('button', { name: 'Turn on' }))

    await waitFor(() => expect(screen.getByText(/The computer could not turn on/)).toBeTruthy())
    expect(screen.getByText('dojo: provision failed: create sandbox rejected')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Turn on' })).toBeTruthy()
  })

  it('lets the USER turn the computer off (ship-first) from the chrome', async () => {
    getDojoSession.mockResolvedValue(session('active'))
    const { fireEvent } = await import('@testing-library/react')
    const dojo = foldDojoEvent('dojo.active', {
      session_id: 'dojo-abc',
      conversation_id: 'conv-1',
    })!
    renderWithIntl(<DojoDesktopScreen dojo={dojo} />)
    const off = await screen.findByRole('button', { name: 'Turn off' })
    // After the teardown the server reports no session.
    getDojoSession.mockResolvedValue(null)
    fireEvent.click(off)
    await waitFor(() => expect(dojoShutdown).toHaveBeenCalledWith('conv-1'))
    await waitFor(() => expect(screen.getByText('The computer is off')).toBeTruthy())
  })

  it('shows the Desktop screen in the Computer without any dojo event when a conversation exists', () => {
    const task = buildTaskFromTrace([], 'neo_run')
    renderWithIntl(
      <NeoComputer
        task={task}
        phase="idle"
        reduce
        showMedia={false}
        legacyOnly
        conversationId="conv-1"
      />,
    )
    expect(screen.getByText('The computer is off')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Turn on' })).toBeTruthy()
  })

  it('maps overlay clicks to desktop pixels through the letterbox (pure math)', () => {
    // 1280x960 desktop in a 640x480 box: exact halves.
    expect(mapToDesktop(320, 240, { left: 0, top: 0, width: 640, height: 480 }, 1280, 960)).toEqual(
      { x: 640, y: 480 },
    )
    // Widescreen box letterboxes horizontally: x offset = (800-640)/2 = 80.
    expect(mapToDesktop(80, 0, { left: 0, top: 0, width: 800, height: 480 }, 1280, 960)).toEqual({
      x: 0,
      y: 0,
    })
    // Outside the visible desktop → null (never a phantom click).
    expect(mapToDesktop(10, 10, { left: 0, top: 0, width: 800, height: 480 }, 1280, 960)).toBeNull()
  })

  it('revokes frame object URLs on unmount', async () => {
    getDojoSession.mockResolvedValue(session('active'))
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const dojo = foldDojoEvent('dojo.active', {
      session_id: 'dojo-abc',
      conversation_id: 'conv-1',
    })!
    const { unmount } = renderWithIntl(<NeoDesktop dojo={dojo} />)
    await waitFor(() => expect(loadDojoFrame).toHaveBeenCalledWith('conv-1'))
    await waitFor(() => expect(screen.getByAltText("Live view of Neo's desktop")).toBeTruthy())
    unmount()
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('blob:authed-frame:conv-1'))
    revoke.mockRestore()
  })
})
