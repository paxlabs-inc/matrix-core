/**
 * Design Mode — shared types.
 *
 * The editor stores every visual tweak as a plain CSS declaration map keyed by
 * a stable CSS selector and a responsive breakpoint. This keeps the data model
 * trivial to serialise (localStorage / JSON export), trivial to render (one
 * injected stylesheet), and trivial to map back to Tailwind utilities on export.
 */

/** Tailwind v4 default breakpoint buckets. `base` = no media query (mobile-first). */
export type Breakpoint = 'base' | 'sm' | 'md' | 'lg' | 'xl' | '2xl'

export interface BreakpointDef {
  key: Breakpoint
  label: string
  /** min-width in px. `0` for `base`. */
  min: number
}

/** Which simulated viewport the canvas is framed to. */
export type PreviewDevice = 'responsive' | 'mobile' | 'tablet' | 'desktop'

export interface DeviceDef {
  key: PreviewDevice
  label: string
  /** Frame width in px, or `null` for the full live viewport. */
  width: number | null
  height: number | null
  /** Breakpoint bucket edits default into when this device is picked. */
  breakpoint: Breakpoint
}

/** Interaction mode for the selection overlay. */
export type EditMode = 'select' | 'move'

/** A map of kebab-case CSS property -> value (e.g. `{ 'padding-left': '12px' }`). */
export type StyleProps = Record<string, string>

/** overrides[selector][breakpoint] = StyleProps */
export type OverrideMap = Record<string, Partial<Record<Breakpoint, StyleProps>>>
