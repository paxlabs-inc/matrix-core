/**
 * Reader-position scroll helpers for the coding chat rail (CODY-01 req 9.2).
 *
 * A message rail should follow new content ONLY while the user is already at
 * (or near) the bottom. When they scroll up to read older turns, following
 * must stop so the view is never yanked away, and it must resume automatically
 * once they return near the bottom. This module is the pure decision, split out
 * from the rail component so it is unit-testable without the client's rendering
 * stack.
 */

/** Distance from the bottom (px) within which the rail keeps auto-following. */
export const NEAR_BOTTOM_PX = 120

/** A minimal view of the scroll geometry the decision needs. */
export interface ScrollGeometry {
  scrollHeight: number
  scrollTop: number
  clientHeight: number
}

/**
 * True when the viewport is within `threshold` px of the bottom, or too short
 * to scroll — the only state in which new content should auto-follow. When the
 * user has scrolled up to read older turns, this is false and following stops
 * until they return near the bottom.
 */
export function isNearBottom(el: ScrollGeometry, threshold = NEAR_BOTTOM_PX): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= threshold
}
