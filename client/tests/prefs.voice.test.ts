import { beforeEach, describe, expect, it } from 'vitest'
import { DEFAULT_PREFS, loadPrefs, savePrefs } from '@/lib/prefs'

describe('voice preferences', () => {
  beforeEach(() => window.localStorage.clear())

  it('defaults to the built-in Mia voice and base style', () => {
    expect(loadPrefs().voice).toEqual(DEFAULT_PREFS.voice)
  })

  it('preserves legacy notification preferences while adding voice defaults', () => {
    window.localStorage.setItem(
      'mx:prefs:v1',
      JSON.stringify({ notif: { completed: false, needsInput: true, failed: true } }),
    )
    expect(loadPrefs()).toEqual({
      notif: { completed: false, needsInput: true, failed: true },
      voice: DEFAULT_PREFS.voice,
    })
  })

  it('round-trips a selected voice and style', () => {
    const prefs = {
      ...DEFAULT_PREFS,
      voice: { voice: 'Chloe', style: 'Calm and concise.' },
    }
    savePrefs(prefs)
    expect(loadPrefs()).toEqual(prefs)
  })
})
