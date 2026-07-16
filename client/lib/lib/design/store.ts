/**
 * Design Mode — global editor state (zustand + localStorage).
 *
 * `enabled` is intentionally NOT persisted: it always boots `false` so server
 * and client render identically (no hydration mismatch) and the editor never
 * pops open on its own. Overrides + the chosen device persist so a layout
 * session survives reloads.
 */
import { create } from 'zustand'
import { persist } from 'zustand/middleware'
import { deviceDef } from './breakpoints'
import type { Breakpoint, EditMode, OverrideMap, PreviewDevice, StyleProps } from './types'

const HISTORY_LIMIT = 100

interface DesignState {
  enabled: boolean
  device: PreviewDevice
  activeBreakpoint: Breakpoint
  mode: EditMode
  selected: string | null
  /** Transient: true while a drag/resize gesture is in flight (not persisted). */
  dragging: boolean
  overrides: OverrideMap
  past: OverrideMap[]
  futureStack: OverrideMap[]

  setEnabled: (v: boolean) => void
  toggleEnabled: () => void
  setDevice: (d: PreviewDevice) => void
  setBreakpoint: (bp: Breakpoint) => void
  setMode: (m: EditMode) => void
  select: (selector: string | null) => void

  setProp: (selector: string, bp: Breakpoint, prop: string, value: string | null) => void
  setProps: (selector: string, bp: Breakpoint, partial: StyleProps) => void
  /** Snapshot current overrides onto the undo stack (call once at drag start). */
  pushHistory: () => void
  /** Mutate overrides WITHOUT touching history (call during a live drag/resize). */
  setPropsLive: (selector: string, bp: Breakpoint, partial: StyleProps) => void
  clearSelector: (selector: string) => void
  clearBreakpoint: (selector: string, bp: Breakpoint) => void
  resetAll: () => void

  undo: () => void
  redo: () => void
}

function cloneOverrides(o: OverrideMap): OverrideMap {
  return JSON.parse(JSON.stringify(o)) as OverrideMap
}

/** Apply a mutation to a deep copy and push the previous state onto history. */
function withHistory(
  state: DesignState,
  mutate: (draft: OverrideMap) => void,
): Partial<DesignState> {
  const next = cloneOverrides(state.overrides)
  mutate(next)
  const past = [...state.past, state.overrides].slice(-HISTORY_LIMIT)
  return { overrides: next, past, futureStack: [] }
}

function setDecl(
  draft: OverrideMap,
  selector: string,
  bp: Breakpoint,
  prop: string,
  value: string | null,
) {
  const buckets = draft[selector] ?? (draft[selector] = {})
  const bucket = buckets[bp] ?? (buckets[bp] = {})
  if (value === null || value === '') {
    delete bucket[prop]
    if (Object.keys(bucket).length === 0) delete buckets[bp]
    if (Object.keys(buckets).length === 0) delete draft[selector]
  } else {
    bucket[prop] = value
  }
}

export const useDesignStore = create<DesignState>()(
  persist(
    (set) => ({
      enabled: false,
      device: 'responsive',
      activeBreakpoint: 'base',
      mode: 'select',
      selected: null,
      dragging: false,
      overrides: {},
      past: [],
      futureStack: [],

      setEnabled: (v) => set({ enabled: v }),
      toggleEnabled: () => set((s) => ({ enabled: !s.enabled })),
      setDevice: (d) => set({ device: d, activeBreakpoint: deviceDef(d).breakpoint }),
      setBreakpoint: (bp) => set({ activeBreakpoint: bp }),
      setMode: (m) => set({ mode: m }),
      select: (selector) => set({ selected: selector }),

      setProp: (selector, bp, prop, value) =>
        set((s) => withHistory(s, (d) => setDecl(d, selector, bp, prop, value))),

      setProps: (selector, bp, partial) =>
        set((s) =>
          withHistory(s, (d) => {
            for (const [prop, value] of Object.entries(partial))
              setDecl(d, selector, bp, prop, value)
          }),
        ),

      pushHistory: () =>
        set((s) => ({ past: [...s.past, s.overrides].slice(-HISTORY_LIMIT), futureStack: [] })),

      setPropsLive: (selector, bp, partial) =>
        set((s) => {
          const next = cloneOverrides(s.overrides)
          for (const [prop, value] of Object.entries(partial))
            setDecl(next, selector, bp, prop, value)
          return { overrides: next }
        }),

      clearSelector: (selector) =>
        set((s) =>
          withHistory(s, (d) => {
            delete d[selector]
          }),
        ),

      clearBreakpoint: (selector, bp) =>
        set((s) =>
          withHistory(s, (d) => {
            if (d[selector]) {
              delete d[selector][bp]
              if (Object.keys(d[selector]).length === 0) delete d[selector]
            }
          }),
        ),

      resetAll: () =>
        set((s) => ({
          overrides: {},
          past: [...s.past, s.overrides].slice(-HISTORY_LIMIT),
          futureStack: [],
        })),

      undo: () =>
        set((s) => {
          if (!s.past.length) return s
          const previous = s.past[s.past.length - 1]
          return {
            overrides: previous,
            past: s.past.slice(0, -1),
            futureStack: [s.overrides, ...s.futureStack].slice(0, HISTORY_LIMIT),
          }
        }),

      redo: () =>
        set((s) => {
          if (!s.futureStack.length) return s
          const next = s.futureStack[0]
          return {
            overrides: next,
            past: [...s.past, s.overrides].slice(-HISTORY_LIMIT),
            futureStack: s.futureStack.slice(1),
          }
        }),
    }),
    {
      name: 'matrix-design-overrides-v1',
      partialize: (s) => ({
        overrides: s.overrides,
        device: s.device,
        activeBreakpoint: s.activeBreakpoint,
      }),
    },
  ),
)
