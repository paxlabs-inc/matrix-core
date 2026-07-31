import { afterEach, describe, expect, it } from 'vitest'
import { buildConnectSrc, buildContentSecurityPolicy } from '@/lib/security/csp'

const originalEnv = { ...process.env }

afterEach(() => {
  process.env = { ...originalEnv }
})

describe('LiveKit content security policy', () => {
  it('allows the configured LiveKit HTTPS and WebSocket origins', () => {
    process.env.NEXT_PUBLIC_LIVEKIT_URL = 'wss://voice.example.test/rtc'

    const sources = buildConnectSrc().split(' ')

    expect(sources).toContain('wss://voice.example.test')
    expect(sources).toContain('https://voice.example.test')
  })

  it('includes both LiveKit transports in the request CSP', () => {
    process.env.NEXT_PUBLIC_LIVEKIT_URL = 'https://voice.example.test'

    const policy = buildContentSecurityPolicy('test-nonce')

    expect(policy).toContain('https://voice.example.test')
    expect(policy).toContain('wss://voice.example.test')
  })

  it('allows authenticated workspace blob previews for images, media, and PDFs', () => {
    const policy = buildContentSecurityPolicy('test-nonce')

    expect(policy).toContain("img-src 'self' data: blob:")
    expect(policy).toContain("media-src 'self' blob:")
    expect(policy).toMatch(/frame-src [^;]*blob:/)
  })
})
