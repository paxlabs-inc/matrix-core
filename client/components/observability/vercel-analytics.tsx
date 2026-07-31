'use client'

import dynamic from 'next/dynamic'

const Analytics = dynamic(() => import('@vercel/analytics/react').then((m) => m.Analytics), {
  ssr: false,
})

/** Lazy-loaded product analytics — avoids blocking the main bundle. */
export function VercelAnalytics() {
  if (process.env.NODE_ENV !== 'production') return null
  return <Analytics />
}
