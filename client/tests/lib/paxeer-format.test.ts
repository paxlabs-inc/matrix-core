import { describe, expect, it } from 'vitest'
import { formatDecimalAmount, formatTokenAmount } from '@/lib/paxeer/format'

describe('Paxeer amount formatting', () => {
  it('converts wei into readable whole-token amounts', () => {
    expect(formatTokenAmount('1000000000000000000')).toBe('1')
    expect(formatTokenAmount('1234500000000000000')).toBe('1.2345')
    expect(formatTokenAmount('-500000000000000000')).toBe('-0.5')
  })

  it('groups large values without losing precision', () => {
    expect(formatTokenAmount('1234567890123456789012')).toBe('1,234.56789')
  })

  it('formats already-converted decimal values without dividing twice', () => {
    expect(formatDecimalAmount('1200345.500000')).toBe('1,200,345.5')
    expect(formatTokenAmount('12.500000')).toBe('12.5')
  })
})
