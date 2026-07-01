/**
 * Design Mode — CSS generation + live injection.
 *
 * Two render modes:
 *  - `responsive`: emit `base` rules then ascending `@media (min-width)` blocks,
 *    so the true viewport drives the cascade (used when the canvas is unframed).
 *  - `flat`: merge every breakpoint up to `flatUpTo` into one un-mediated block,
 *    so a framed device preview shows the correct per-device result even though
 *    the real viewport width does not match the frame.
 *
 * The live overlay injects with `!important` so edits reliably win over the
 * app's existing Tailwind utilities (what you see is what you tweaked). Exported
 * CSS omits `!important` for clean source.
 */
import { BREAKPOINT_ORDER, bpMin, breakpointsUpTo } from './breakpoints'
import type { Breakpoint, OverrideMap, StyleProps } from './types'

const STYLE_ID = 'mx-design-overrides'

function declBlock(props: StyleProps, important: boolean): string {
  const bang = important ? ' !important' : ''
  return Object.entries(props)
    .filter(([, v]) => v !== '' && v != null)
    .map(([k, v]) => `${k}:${v}${bang};`)
    .join('')
}

function mergeUpTo(buckets: Partial<Record<Breakpoint, StyleProps>>, upTo: Breakpoint): StyleProps {
  const out: StyleProps = {}
  for (const bp of breakpointsUpTo(upTo)) {
    const b = buckets[bp]
    if (b) Object.assign(out, b)
  }
  return out
}

export interface BuildOptions {
  mode: 'responsive' | 'flat'
  /** Required when `mode === 'flat'`. */
  flatUpTo?: Breakpoint
  important?: boolean
}

export function buildCSS(overrides: OverrideMap, opts: BuildOptions): string {
  const important = opts.important ?? false
  const lines: string[] = []

  if (opts.mode === 'flat') {
    const upTo = opts.flatUpTo ?? 'base'
    for (const [selector, buckets] of Object.entries(overrides)) {
      const props = mergeUpTo(buckets, upTo)
      const block = declBlock(props, important)
      if (block) lines.push(`${selector}{${block}}`)
    }
    return lines.join('\n')
  }

  // responsive: base first, then ascending media queries.
  for (const bp of BREAKPOINT_ORDER) {
    const inner: string[] = []
    for (const [selector, buckets] of Object.entries(overrides)) {
      const props = buckets[bp]
      if (!props) continue
      const block = declBlock(props, important)
      if (block) inner.push(`${selector}{${block}}`)
    }
    if (!inner.length) continue
    if (bp === 'base') {
      lines.push(inner.join('\n'))
    } else {
      lines.push(`@media (min-width:${bpMin(bp)}px){\n${inner.join('\n')}\n}`)
    }
  }
  return lines.join('\n')
}

/** Insert or update the single design-overrides <style> tag in <head>. */
export function injectCSS(css: string): void {
  if (typeof document === 'undefined') return
  let el = document.getElementById(STYLE_ID) as HTMLStyleElement | null
  if (!el) {
    el = document.createElement('style')
    el.id = STYLE_ID
    el.setAttribute('data-mx-ui', 'true')
    document.head.appendChild(el)
  }
  el.textContent = css
}

export function clearInjectedCSS(): void {
  if (typeof document === 'undefined') return
  const el = document.getElementById(STYLE_ID)
  if (el) el.textContent = ''
}
