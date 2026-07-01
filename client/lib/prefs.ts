'use client'

/**
 * Local preferences — small, client-only settings that don't (yet) have a
 * daemon write endpoint, so they live in localStorage and sync across tabs.
 * Today this is just which notification categories the user wants to see;
 * the Settings page writes them and the notification panel reads them.
 */
import { useCallback, useEffect, useState } from 'react'

export interface NotifPrefs {
  /** A task finished and signed its receipt. */
  completed: boolean
  /** A task paused and needs the user's decision. */
  needsInput: boolean
  /** A task failed. */
  failed: boolean
}

export interface LocalPrefs {
  notif: NotifPrefs
}

/** A shallow patch applied by usePrefs().update — `notif` may be partial and
 *  is merged into the current value. */
export interface PrefsPatch {
  notif?: Partial<NotifPrefs>
}

const KEY = 'mx:prefs:v1'
const EVENT = 'mx:prefs'

export const DEFAULT_PREFS: LocalPrefs = {
  notif: { completed: true, needsInput: true, failed: true },
}

export function loadPrefs(): LocalPrefs {
  if (typeof window === 'undefined') return DEFAULT_PREFS
  try {
    const raw = window.localStorage.getItem(KEY)
    if (!raw) return DEFAULT_PREFS
    const parsed = JSON.parse(raw) as Partial<LocalPrefs>
    return { notif: { ...DEFAULT_PREFS.notif, ...(parsed.notif ?? {}) } }
  } catch {
    return DEFAULT_PREFS
  }
}

export function savePrefs(p: LocalPrefs): void {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.setItem(KEY, JSON.stringify(p))
    window.dispatchEvent(new Event(EVENT))
  } catch {
    /* storage unavailable (private mode / quota) — preferences stay in-memory */
  }
}

/**
 * Reactive accessor. Starts from defaults on the server and the first client
 * paint (so hydration is deterministic), then loads the stored value and
 * subscribes to same-tab (custom event) + cross-tab (storage event) updates.
 */
export function usePrefs(): [LocalPrefs, (next: PrefsPatch) => void] {
  const [prefs, setPrefs] = useState<LocalPrefs>(DEFAULT_PREFS)

  useEffect(() => {
    setPrefs(loadPrefs())
    const sync = () => setPrefs(loadPrefs())
    window.addEventListener(EVENT, sync)
    window.addEventListener('storage', sync)
    return () => {
      window.removeEventListener(EVENT, sync)
      window.removeEventListener('storage', sync)
    }
  }, [])

  const update = useCallback((next: PrefsPatch) => {
    setPrefs((cur) => {
      const merged: LocalPrefs = { ...cur, notif: { ...cur.notif, ...(next.notif ?? {}) } }
      savePrefs(merged)
      return merged
    })
  }, [])

  return [prefs, update]
}
