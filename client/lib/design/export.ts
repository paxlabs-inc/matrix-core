/**
 * Design Mode — export.
 *
 * Three portable outputs from the same override map:
 *  - CSS: clean responsive stylesheet (no !important), paste into globals.css.
 *  - Tailwind: best-effort utility classes per selector + breakpoint, with a
 *    guaranteed arbitrary-property fallback so every declaration is covered.
 *  - JSON: the raw override map (commit as a design spec / re-import later).
 */
import { BREAKPOINT_ORDER, bpPrefix } from './breakpoints'
import { buildCSS } from './css-engine'
import { selectorLabel } from './selector'
import type { Breakpoint, OverrideMap, StyleProps } from './types'

export function exportCSS(overrides: OverrideMap): string {
  return buildCSS(overrides, { mode: 'responsive', important: false })
}

export function exportJSON(overrides: OverrideMap): string {
  return JSON.stringify(overrides, null, 2)
}

const JUSTIFY: Record<string, string> = {
  'flex-start': 'justify-start',
  center: 'justify-center',
  'flex-end': 'justify-end',
  'space-between': 'justify-between',
  'space-around': 'justify-around',
  'space-evenly': 'justify-evenly',
}
const ALIGN: Record<string, string> = {
  'flex-start': 'items-start',
  center: 'items-center',
  'flex-end': 'items-end',
  stretch: 'items-stretch',
  baseline: 'items-baseline',
}
const DISPLAY: Record<string, string> = {
  flex: 'flex',
  'inline-flex': 'inline-flex',
  grid: 'grid',
  block: 'block',
  'inline-block': 'inline-block',
  inline: 'inline',
  none: 'hidden',
}
const TEXT_ALIGN: Record<string, string> = {
  left: 'text-left',
  center: 'text-center',
  right: 'text-right',
  justify: 'text-justify',
}

/** Arbitrary value, with spaces escaped to underscores per Tailwind syntax. */
function arb(value: string): string {
  return value.trim().replace(/\s+/g, '_')
}

function signed(prefix: string, value: string): string {
  const neg = value.trim().startsWith('-')
  const v = neg ? value.trim().slice(1) : value.trim()
  return `${neg ? '-' : ''}${prefix}-[${arb(v)}]`
}

/** Map a single CSS declaration to one Tailwind class (friendly or arbitrary). */
function declToClass(prop: string, value: string): string {
  switch (prop) {
    case 'display':
      return DISPLAY[value] ?? `[display:${arb(value)}]`
    case 'flex-direction':
      return value === 'column'
        ? 'flex-col'
        : value === 'row'
          ? 'flex-row'
          : `[flex-direction:${arb(value)}]`
    case 'flex-wrap':
      return value === 'wrap'
        ? 'flex-wrap'
        : value === 'nowrap'
          ? 'flex-nowrap'
          : `[flex-wrap:${arb(value)}]`
    case 'justify-content':
      return JUSTIFY[value] ?? `[justify-content:${arb(value)}]`
    case 'align-items':
      return ALIGN[value] ?? `[align-items:${arb(value)}]`
    case 'gap':
      return `gap-[${arb(value)}]`
    case 'padding':
      return `p-[${arb(value)}]`
    case 'padding-top':
      return `pt-[${arb(value)}]`
    case 'padding-right':
      return `pr-[${arb(value)}]`
    case 'padding-bottom':
      return `pb-[${arb(value)}]`
    case 'padding-left':
      return `pl-[${arb(value)}]`
    case 'margin':
      return signed('m', value)
    case 'margin-top':
      return signed('mt', value)
    case 'margin-right':
      return signed('mr', value)
    case 'margin-bottom':
      return signed('mb', value)
    case 'margin-left':
      return signed('ml', value)
    case 'width':
      return `w-[${arb(value)}]`
    case 'height':
      return `h-[${arb(value)}]`
    case 'min-width':
      return `min-w-[${arb(value)}]`
    case 'min-height':
      return `min-h-[${arb(value)}]`
    case 'max-width':
      return `max-w-[${arb(value)}]`
    case 'max-height':
      return `max-h-[${arb(value)}]`
    case 'position':
      return ['static', 'relative', 'absolute', 'fixed', 'sticky'].includes(value)
        ? value
        : `[position:${arb(value)}]`
    case 'top':
      return signed('top', value)
    case 'right':
      return signed('right', value)
    case 'bottom':
      return signed('bottom', value)
    case 'left':
      return signed('left', value)
    case 'z-index':
      return `z-[${arb(value)}]`
    case 'order':
      return `order-[${arb(value)}]`
    case 'font-size':
      return `text-[${arb(value)}]`
    case 'font-weight':
      return `font-[${arb(value)}]`
    case 'line-height':
      return `leading-[${arb(value)}]`
    case 'letter-spacing':
      return `tracking-[${arb(value)}]`
    case 'text-align':
      return TEXT_ALIGN[value] ?? `[text-align:${arb(value)}]`
    case 'text-transform':
      return ['uppercase', 'lowercase', 'capitalize'].includes(value)
        ? value
        : `[text-transform:${arb(value)}]`
    case 'color':
      return `text-[${arb(value)}]`
    case 'background-color':
      return `bg-[${arb(value)}]`
    case 'border-radius':
      return `rounded-[${arb(value)}]`
    case 'opacity':
      return `opacity-[${arb(value)}]`
    case 'overflow':
      return ['auto', 'hidden', 'visible', 'scroll'].includes(value)
        ? `overflow-${value}`
        : `[overflow:${arb(value)}]`
    default:
      return `[${prop}:${arb(value)}]`
  }
}

function classesForBucket(props: StyleProps, bp: Breakpoint): string[] {
  const prefix = bpPrefix(bp)
  return Object.entries(props)
    .filter(([, v]) => v !== '' && v != null)
    .map(([prop, value]) => `${prefix}${declToClass(prop, value)}`)
}

export interface TailwindExportEntry {
  selector: string
  label: string
  classes: string
}

export function exportTailwind(overrides: OverrideMap): TailwindExportEntry[] {
  const entries: TailwindExportEntry[] = []
  for (const [selector, buckets] of Object.entries(overrides)) {
    const classes: string[] = []
    for (const bp of BREAKPOINT_ORDER) {
      const b = buckets[bp]
      if (b) classes.push(...classesForBucket(b, bp))
    }
    if (classes.length) {
      entries.push({ selector, label: selectorLabel(selector), classes: classes.join(' ') })
    }
  }
  return entries
}

export function exportTailwindText(overrides: OverrideMap): string {
  return exportTailwind(overrides)
    .map((e) => `/* ${e.label}  —  ${e.selector} */\n${e.classes}`)
    .join('\n\n')
}
