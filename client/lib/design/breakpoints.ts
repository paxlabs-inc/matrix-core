/**
 * Design Mode — breakpoint + device tables.
 *
 * Breakpoint mins mirror Tailwind v4 defaults so exported responsive prefixes
 * (`sm:`, `md:`, ...) land at the exact same widths the app already uses.
 */
import type { Breakpoint, BreakpointDef, DeviceDef, PreviewDevice } from './types'

export const BREAKPOINTS: BreakpointDef[] = [
  { key: 'base', label: 'Base', min: 0 },
  { key: 'sm', label: 'sm', min: 640 },
  { key: 'md', label: 'md', min: 768 },
  { key: 'lg', label: 'lg', min: 1024 },
  { key: 'xl', label: 'xl', min: 1280 },
  { key: '2xl', label: '2xl', min: 1536 },
]

export const BREAKPOINT_ORDER: Breakpoint[] = ['base', 'sm', 'md', 'lg', 'xl', '2xl']

const MIN_BY_KEY: Record<Breakpoint, number> = {
  base: 0,
  sm: 640,
  md: 768,
  lg: 1024,
  xl: 1280,
  '2xl': 1536,
}

export function bpMin(bp: Breakpoint): number {
  return MIN_BY_KEY[bp]
}

/** The Tailwind prefix for a breakpoint (`base` has none). */
export function bpPrefix(bp: Breakpoint): string {
  return bp === 'base' ? '' : `${bp}:`
}

/** Every breakpoint at or below `bp`, in ascending order (for flat cascade merge). */
export function breakpointsUpTo(bp: Breakpoint): Breakpoint[] {
  const idx = BREAKPOINT_ORDER.indexOf(bp)
  return BREAKPOINT_ORDER.slice(0, idx + 1)
}

export const DEVICES: DeviceDef[] = [
  { key: 'responsive', label: 'Responsive', width: null, height: null, breakpoint: 'base' },
  { key: 'mobile', label: 'Mobile', width: 390, height: 844, breakpoint: 'base' },
  { key: 'tablet', label: 'Tablet', width: 820, height: 1180, breakpoint: 'md' },
  { key: 'desktop', label: 'Desktop', width: 1280, height: 832, breakpoint: 'xl' },
]

export function deviceDef(key: PreviewDevice): DeviceDef {
  return DEVICES.find((d) => d.key === key) ?? DEVICES[0]
}
