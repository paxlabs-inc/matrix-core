'use client'

/**
 * useNotifications — turns live runs + receipts into a read/unread-tracked
 * notification feed. Read state is persisted in localStorage so dismissals
 * survive reloads; category visibility comes from the user's preferences.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'
import type { Run, Receipt } from '@/lib/matrix-data'
import { buildNotifications, type Notification } from '@/lib/notifications'
import { usePrefs } from '@/lib/prefs'

const READ_KEY = 'mx:notif:read:v1'

function loadRead(): Set<string> {
  if (typeof window === 'undefined') return new Set()
  try {
    const raw = window.localStorage.getItem(READ_KEY)
    return new Set(raw ? (JSON.parse(raw) as string[]) : [])
  } catch {
    return new Set()
  }
}

function saveRead(ids: Set<string>): void {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.setItem(READ_KEY, JSON.stringify([...ids]))
  } catch {
    /* storage unavailable — read state stays in-memory for this session */
  }
}

export interface UseNotifications {
  items: Notification[]
  unreadCount: number
  readIds: Set<string>
  markAllRead: () => void
}

export function useNotifications(runs: Run[], receipts: Receipt[]): UseNotifications {
  const [prefs] = usePrefs()
  const [readIds, setReadIds] = useState<Set<string>>(() => new Set())

  // Load persisted read state after mount (keeps SSR markup deterministic).
  useEffect(() => {
    setReadIds(loadRead())
  }, [])

  const items = useMemo(
    () => buildNotifications(runs, receipts, prefs.notif),
    [runs, receipts, prefs.notif],
  )

  const unreadCount = useMemo(
    () => items.reduce((n, it) => (readIds.has(it.id) ? n : n + 1), 0),
    [items, readIds],
  )

  const markAllRead = useCallback(() => {
    setReadIds(() => {
      const next = new Set(items.map((i) => i.id))
      saveRead(next)
      return next
    })
  }, [items])

  return { items, unreadCount, readIds, markAllRead }
}
