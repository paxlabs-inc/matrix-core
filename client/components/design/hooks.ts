'use client'

/**
 * Design Mode — shared React hooks.
 */
import { useEffect, useState } from 'react'
import { resolveSelector } from '@/lib/design/selector'
import { useDesignStore } from '@/lib/design/store'

/** True only after first client paint — guards hydration-sensitive UI. */
export function useMounted(): boolean {
  const [mounted, setMounted] = useState(false)
  useEffect(() => setMounted(true), [])
  return mounted
}

/**
 * Resolve the currently selected element and expose a `computed(prop)` reader.
 * Re-resolves whenever the selection or override map changes (so placeholders
 * reflect the live, override-applied render).
 */
export function useSelectedElement(): {
  el: HTMLElement | null
  computed: (prop: string) => string
} {
  const selected = useDesignStore((s) => s.selected)
  const overrides = useDesignStore((s) => s.overrides)
  const [el, setEl] = useState<HTMLElement | null>(null)

  useEffect(() => {
    setEl(resolveSelector(selected))
    // overrides included so a structural change re-resolves the node.
  }, [selected, overrides])

  const computed = (prop: string): string => {
    if (!el || typeof window === 'undefined') return ''
    try {
      return window.getComputedStyle(el).getPropertyValue(prop).trim()
    } catch {
      return ''
    }
  }

  return { el, computed }
}
