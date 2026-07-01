/**
 * Design Mode — value resolution helpers (pure).
 *
 * Inspector fields show the *effective* value for the active breakpoint: the
 * override defined directly on that breakpoint if present, otherwise the value
 * inherited from a smaller breakpoint (mobile-first cascade), otherwise the
 * element's live computed style as a placeholder.
 */
import { breakpointsUpTo } from './breakpoints'
import type { Breakpoint, OverrideMap } from './types'

/** The value set directly on this breakpoint bucket (no cascade). */
export function ownValue(
  overrides: OverrideMap,
  selector: string | null,
  bp: Breakpoint,
  prop: string,
): string | undefined {
  if (!selector) return undefined
  return overrides[selector]?.[bp]?.[prop]
}

/** The cascaded override value at `bp` (this bucket or the nearest smaller one). */
export function effectiveValue(
  overrides: OverrideMap,
  selector: string | null,
  bp: Breakpoint,
  prop: string,
): string | undefined {
  if (!selector) return undefined
  const buckets = overrides[selector]
  if (!buckets) return undefined
  let found: string | undefined
  for (const b of breakpointsUpTo(bp)) {
    const v = buckets[b]?.[prop]
    if (v !== undefined) found = v
  }
  return found
}

/** Parse a leading numeric portion (`"12px"` -> 12). */
export function parseNum(value: string | undefined): number | null {
  if (value == null) return null
  const m = /-?\d*\.?\d+/.exec(value)
  return m ? Number(m[0]) : null
}

/** Extract translate(x,y) from a transform string. */
export function parseTranslate(transform: string | undefined): { x: number; y: number } {
  if (!transform) return { x: 0, y: 0 }
  const m = /translate\(\s*(-?\d*\.?\d+)px\s*,\s*(-?\d*\.?\d+)px\s*\)/.exec(transform)
  if (!m) return { x: 0, y: 0 }
  return { x: Number(m[1]), y: Number(m[2]) }
}

export function formatTranslate(x: number, y: number): string {
  return `translate(${Math.round(x)}px, ${Math.round(y)}px)`
}
