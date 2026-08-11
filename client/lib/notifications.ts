/**
 * Notifications — derived, not fetched. Centra AI has no dedicated notifications
 * endpoint in v1, so the panel composes a feed from the live domain data the
 * dashboard already holds: runs that need the user (needs_input) or failed,
 * and receipts that just got signed. Read/unread state is tracked client-side
 * (see hooks/useNotifications). Category visibility honours the user's
 * notification preferences.
 */
import type { Run, Receipt } from '@/lib/matrix-data'
import type { NotifPrefs } from '@/lib/prefs'

export type NotifKind = 'completed' | 'needs_input' | 'failed'

export interface Notification {
  id: string
  kind: NotifKind
  /** The run/receipt title, already in the user's own words. */
  subject: string
  /** ISO timestamp used for ordering + relative-time rendering. */
  at: string
}

function timeOf(iso: string | undefined): number {
  if (!iso) return 0
  const t = new Date(iso).getTime()
  return Number.isNaN(t) ? 0 : t
}

/**
 * Build the notification feed from live runs + receipts, filtered by the
 * user's category preferences and sorted newest-first.
 */
export function buildNotifications(
  runs: Run[],
  receipts: Receipt[],
  prefs: NotifPrefs,
): Notification[] {
  const out: Notification[] = []

  if (prefs.completed) {
    for (const r of receipts) {
      out.push({ id: `rcpt-${r.id}`, kind: 'completed', subject: r.title, at: r.signedAt })
    }
  }

  for (const run of runs) {
    if (run.status === 'needs_input' && prefs.needsInput) {
      out.push({
        id: `needs-${run.id}`,
        kind: 'needs_input',
        subject: run.title,
        at: run.createdAt,
      })
    } else if (run.status === 'failed' && prefs.failed) {
      out.push({ id: `fail-${run.id}`, kind: 'failed', subject: run.title, at: run.createdAt })
    }
  }

  out.sort((a, b) => timeOf(b.at) - timeOf(a.at))
  return out
}
