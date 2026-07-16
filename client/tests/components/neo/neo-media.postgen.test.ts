import { describe, expect, it } from 'vitest'
import {
  composeImageSuggestion,
  composeImageTweak,
  composeImageVariations,
  suggestImagePrompts,
} from '@/components/matrix/neo/neo-media'

describe('suggestImagePrompts', () => {
  it('returns a stable non-empty set for an empty prompt', () => {
    const a = suggestImagePrompts(undefined, 5)
    const b = suggestImagePrompts('', 5)
    expect(a.length).toBeGreaterThan(0)
    expect(a.length).toBeLessThanOrEqual(5)
    expect(a).toEqual(b)
  })

  it('adds portrait-aware pivots when the prompt mentions a person', () => {
    const s = suggestImagePrompts('portrait of a woman in a cafe', 8)
    expect(s.some((x) => /portrait/i.test(x))).toBe(true)
  })

  it('adds logo-aware pivots for brand marks', () => {
    const s = suggestImagePrompts('minimal logo for a fintech brand', 8)
    expect(s.some((x) => /logo|mark|badge/i.test(x))).toBe(true)
  })

  it('dedupes case-insensitively and respects the limit', () => {
    const s = suggestImagePrompts('neon cyber city skyline at night', 3)
    expect(s.length).toBe(3)
    const lower = s.map((x) => x.toLowerCase())
    expect(new Set(lower).size).toBe(lower.length)
  })
})

describe('composeImageTweak', () => {
  it('embeds the media ref and the user brief for edit_image', () => {
    const out = composeImageTweak('/media/abc.png', '  make the sky stormier  ')
    expect(out).toContain('/media/abc.png')
    expect(out).toContain('make the sky stormier')
    expect(out).toMatch(/edit_image/i)
  })
})

describe('composeImageVariations', () => {
  it('asks for a bounded number of variations with the media ref', () => {
    const out = composeImageVariations('/media/x.png', 'a red fox', 3)
    expect(out).toContain('/media/x.png')
    expect(out).toContain('3 variations')
    expect(out).toContain('a red fox')
  })

  it('clamps the count into 2..4', () => {
    expect(composeImageVariations('/media/x.png', undefined, 99)).toContain('4 variations')
    expect(composeImageVariations('/media/x.png', undefined, 1)).toContain('2 variations')
  })
})

describe('composeImageSuggestion', () => {
  it('extends the original prompt when one exists', () => {
    const out = composeImageSuggestion('/media/z.png', 'warmer golden-hour palette', 'red fox')
    expect(out).toContain('Create an image: red fox, warmer golden-hour palette')
    expect(out).toContain('/media/z.png')
  })

  it('falls back to the direction alone when there is no original prompt', () => {
    const out = composeImageSuggestion('/media/z.png', 'cinematic lighting')
    expect(out).toContain('Create an image: cinematic lighting')
    expect(out).toContain('/media/z.png')
  })
})
