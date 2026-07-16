import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import type { SSEUpdate } from '@/lib/realtime/sse'

/* Capture the onUpdate callback the hook registers so the test can
 * drive the SSE lifecycle deterministically without a real stream. */
let captured: ((u: SSEUpdate) => void) | null = null
const closeSpy = vi.fn()
const subscribeSpy = vi.fn((opts: { intentId?: string; onUpdate: (u: SSEUpdate) => void }) => {
  captured = opts.onUpdate
  return { close: closeSpy }
})

vi.mock('@/lib/api/events', () => ({
  subscribeEvents: (opts: { intentId?: string; onUpdate: (u: SSEUpdate) => void }) =>
    subscribeSpy(opts),
}))

import { useRunEvents } from '@/hooks/api/useRunEvents'

function emit(u: SSEUpdate) {
  act(() => {
    captured?.(u)
  })
}

beforeEach(() => {
  captured = null
  subscribeSpy.mockClear()
  closeSpy.mockClear()
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('useRunEvents', () => {
  it('does not subscribe when intentId is null and reports closed', () => {
    const { result } = renderHook(() => useRunEvents(null))
    expect(subscribeSpy).not.toHaveBeenCalled()
    expect(result.current.connection).toBe('closed')
  })

  it('subscribes by intent_id and tracks the open→event lifecycle', () => {
    const { result } = renderHook(() => useRunEvents('intent_x'))
    expect(subscribeSpy).toHaveBeenCalledTimes(1)
    expect(subscribeSpy.mock.calls[0][0].intentId).toBe('intent_x')
    expect(result.current.connection).toBe('connecting')

    emit({ kind: 'open' })
    expect(result.current.connection).toBe('open')

    emit({
      kind: 'event',
      event: { seq: 1, ts: 't1', phase: 'walk', type: 'plan.tool.dispatch', fields: {} },
    })
    expect(result.current.events).toHaveLength(1)
    expect(result.current.events[0].type).toBe('plan.tool.dispatch')
  })

  it('surfaces a sequence gap as a synthetic sse.gap event', () => {
    const { result } = renderHook(() => useRunEvents('intent_x'))
    emit({ kind: 'open' })
    emit({ kind: 'gap', from: 2, to: 7 })
    const gap = result.current.events.at(-1)
    expect(gap?.type).toBe('sse.gap')
    expect(gap?.fields).toMatchObject({ from: 2, to: 7 })
  })

  it('tracks reconnect attempts and errors', () => {
    const { result } = renderHook(() => useRunEvents('intent_x'))
    emit({ kind: 'reconnecting', attempt: 3 })
    expect(result.current.connection).toBe('reconnecting')
    expect(result.current.reconnectAttempt).toBe(3)

    emit({ kind: 'error', error: new Error('stream down') })
    expect(result.current.connection).toBe('error')
    expect(result.current.lastError).toBe('stream down')
  })

  it('closes the underlying connection on unmount', () => {
    const { unmount } = renderHook(() => useRunEvents('intent_x'))
    unmount()
    expect(closeSpy).toHaveBeenCalled()
  })

  it('resets the stream when the intent changes', () => {
    const { result, rerender } = renderHook(({ id }) => useRunEvents(id), {
      initialProps: { id: 'intent_a' as string | null },
    })
    emit({ kind: 'open' })
    emit({
      kind: 'event',
      event: { seq: 1, ts: 't', phase: 'walk', type: 'a', fields: {} },
    })
    expect(result.current.events).toHaveLength(1)

    rerender({ id: 'intent_b' })
    // New subscription, fresh state.
    expect(closeSpy).toHaveBeenCalled()
    expect(result.current.events).toHaveLength(0)
    expect(result.current.connection).toBe('connecting')
  })
})
