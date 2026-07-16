import { describe, expect, it } from 'vitest'
import { sanitizeSvgMarkup } from '@/lib/security/sanitize'

describe('sanitizeSvgMarkup', () => {
  it('preserves path geometry from trusted icon fragments', () => {
    const body = '<path d="M12 5v14"/><path d="M5 12h14"/>'
    const out = sanitizeSvgMarkup(body)
    expect(out).toContain('M12 5v14')
    expect(out).toContain('M5 12h14')
  })

  it('preserves circles with currentColor fill', () => {
    const body = '<circle cx="12" cy="12" r="2" fill="currentColor" stroke="none"/>'
    const out = sanitizeSvgMarkup(body)
    expect(out).toContain('currentColor')
    expect(out).toContain('cx="12"')
  })

  it('returns empty string for empty input', () => {
    expect(sanitizeSvgMarkup('')).toBe('')
    expect(sanitizeSvgMarkup('   ')).toBe('')
  })
})
