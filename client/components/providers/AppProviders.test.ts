import { describe, expect, it } from 'vitest'
import { isWorkforceRoute } from './AppProviders'

describe('isWorkforceRoute', () => {
  it('selects only the localized Workforce route tree', () => {
    expect(isWorkforceRoute('/en/workforce', 'en')).toBe(true)
    expect(isWorkforceRoute('/de/workforce/receipt/123', 'de')).toBe(true)
    expect(isWorkforceRoute('/en', 'en')).toBe(false)
    expect(isWorkforceRoute('/en/workforce-lab', 'en')).toBe(false)
    expect(isWorkforceRoute('/de/workforce', 'en')).toBe(false)
  })
})
