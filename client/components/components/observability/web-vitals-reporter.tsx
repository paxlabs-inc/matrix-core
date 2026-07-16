'use client'

/**
 * Tiny client component that boots the web-vitals listeners on mount.
 * Rendering it as a sibling of the main tree means the listener
 * registration is decoupled from feature components — the dashboard
 * never has to think about it.
 */
import { useEffect } from 'react'
import { startWebVitals } from '@/lib/observability/web-vitals'

export function WebVitalsReporter() {
  useEffect(() => {
    startWebVitals()
  }, [])
  return null
}
