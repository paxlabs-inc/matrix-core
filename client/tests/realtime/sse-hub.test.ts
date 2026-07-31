import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { subscribeSSEHub, _resetSSEHubForTests } from '@/lib/realtime/sse-hub'
import type { SSEUpdate } from '@/lib/realtime/sse'

vi.mock('@/lib/realtime/sse', () => ({
  startSSE: vi.fn((opts: { onUpdate: (u: SSEUpdate) => void }) => {
    const listeners = [opts.onUpdate]
    return {
      close: vi.fn(),
      _emit: (u: SSEUpdate) => listeners.forEach((l) => l(u)),
    }
  }),
}))

import { startSSE } from '@/lib/realtime/sse'

describe('subscribeSSEHub', () => {
  beforeEach(() => {
    _resetSSEHubForTests()
    vi.clearAllMocks()
  })

  afterEach(() => {
    _resetSSEHubForTests()
  })

  it('shares one startSSE call for identical stream keys', () => {
    const a: SSEUpdate[] = []
    const b: SSEUpdate[] = []
    const h1 = subscribeSSEHub({ path: '/events?intent_id=abc' }, (u) => a.push(u))
    const h2 = subscribeSSEHub({ path: '/events?intent_id=abc' }, (u) => b.push(u))
    expect(startSSE).toHaveBeenCalledTimes(1)
    h1.close()
    expect(startSSE).toHaveBeenCalledTimes(1)
    h2.close()
  })

  it('opens separate connections for different paths', () => {
    subscribeSSEHub({ path: '/events?intent_id=a' }, () => {})
    subscribeSSEHub({ path: '/events?intent_id=b' }, () => {})
    expect(startSSE).toHaveBeenCalledTimes(2)
  })
})
