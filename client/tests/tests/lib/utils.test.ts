import { describe, it, expect } from 'vitest'
import { cn } from '@/lib/utils'

describe('cn', () => {
  it('merges class names', () => {
    expect(cn('a', 'b')).toBe('a b')
  })

  it('deduplicates conflicting Tailwind classes', () => {
    // tailwind-merge removes the earlier conflicting class
    expect(cn('p-4', 'p-2')).toBe('p-2')
  })

  it('handles conditional classes', () => {
    expect(cn('base', false && 'excluded', 'included')).toBe('base included')
  })

  it('handles undefined and null gracefully', () => {
    expect(cn('a', undefined, null, 'b')).toBe('a b')
  })
})
