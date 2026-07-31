/**
 * CODY-01 req 9.2 — reader-position following. The rail follows new content
 * only while the user is near the bottom; scrolling up to read older turns
 * stops following, and returning near the bottom resumes it. These assert the
 * pure decision the rail's follow effect is driven by.
 */
import { describe, it, expect } from 'vitest'
import { isNearBottom, NEAR_BOTTOM_PX } from '@/lib/cody/scroll'

describe('isNearBottom — reader-position follow decision', () => {
  it('follows when pinned exactly at the bottom', () => {
    expect(isNearBottom({ scrollHeight: 2000, scrollTop: 1400, clientHeight: 600 })).toBe(true)
  })

  it('follows within the near-bottom threshold', () => {
    // 100px from the bottom, under the 120px default.
    expect(isNearBottom({ scrollHeight: 2000, scrollTop: 1300, clientHeight: 600 })).toBe(true)
  })

  it('stops following once the user scrolls up past the threshold', () => {
    // 400px from the bottom — the user is reading older turns.
    expect(isNearBottom({ scrollHeight: 2000, scrollTop: 1000, clientHeight: 600 })).toBe(false)
  })

  it('resumes following the moment they return near the bottom', () => {
    const geo = { scrollHeight: 2000, scrollTop: 900, clientHeight: 600 }
    expect(isNearBottom(geo)).toBe(false)
    // Scrolled back down to within the band.
    expect(isNearBottom({ ...geo, scrollTop: 1290 })).toBe(true)
  })

  it('always follows a viewport too short to scroll', () => {
    expect(isNearBottom({ scrollHeight: 500, scrollTop: 0, clientHeight: 600 })).toBe(true)
  })

  it('honours an explicit threshold', () => {
    const geo = { scrollHeight: 2000, scrollTop: 1360, clientHeight: 600 }
    // 40px from bottom: inside 120 default, outside a tight 20px band.
    expect(isNearBottom(geo)).toBe(true)
    expect(isNearBottom(geo, 20)).toBe(false)
    expect(NEAR_BOTTOM_PX).toBe(120)
  })
})
