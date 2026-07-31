import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen, fireEvent } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'
import type { AutomatrixInboxItem, AutomatrixQueueItem } from '@/lib/api/automatrix'

/* -------------------------------------------------------------------------- */
/*  AUTO-01 — Proactive Neo opportunity/result text stays readable on a       */
/*  narrow screen (spec/launch-readiness req.14).                             */
/* -------------------------------------------------------------------------- */
/*  Renders the REAL AutomatrixSection (its real ReviewDialog / QueueRow /    */
/*  InboxRow render code) through the real i18n catalogue. The ONE mocked     */
/*  boundary is the data-fetch hook module (the network edge) so we can feed  */
/*  adversarial long-URL / long-identifier / code-like / prose fixtures and   */
/*  assert every user-visible proactive text node carries the prose-safe      */
/*  overflow-wrap rule (14.1/14.2) and the panel — not the page — owns the    */
/*  vertical scroll (14.3).                                                   */

const LONG_URL =
  'https://paxscan.paxeer.app/tx/0xabc1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcd?tab=logs&filter=transfers'
const LONG_ID =
  'matrix://opportunity/paxeer-defi-guardian-rebalance-0xDEADBEEFCAFEBABE0123456789ABCDEF01234567@0.1.0'
const CODE_LIKE =
  'result.pool.reserves[0]==0x0000000000000000000000000000000000000000000000000de0b6b3a7640000'

const queueItem: AutomatrixQueueItem = {
  uri: LONG_ID,
  summary: `Rebalance your stable position — see ${LONG_URL}`,
  rationale: `Detected drift in pool ${LONG_ID} where ${CODE_LIKE}`,
  financial: true,
  confidence: 0.82,
  status: 'pending',
  created_at: '2026-07-16T10:00:00Z',
}

const inboxItem: AutomatrixInboxItem = {
  id: 'inbox-1',
  opportunity_summary: `Neo finished checking ${LONG_URL}`,
  result_summary: `Outcome recorded at ${LONG_ID}; ${CODE_LIKE}`,
  conversation_id: 'conv-1',
  created_at: '2026-07-16T11:00:00Z',
  read: false,
}

vi.mock('@/hooks/api/useAutomatrix', () => ({
  useAutomatrixSettings: () => ({ data: { enabled: true }, isSuccess: true, isLoading: false }),
  useAutomatrixInbox: () => ({ data: { items: [inboxItem], unread: 1 }, isLoading: false }),
  useAutomatrixQueue: () => ({ data: [queueItem], isLoading: false }),
  useSetAutomatrixEnabled: () => ({ mutate: vi.fn(), isPending: false }),
  useDismissOpportunity: () => ({ mutate: vi.fn(), isPending: false }),
  useApproveOpportunity: () => ({ mutate: vi.fn(), isPending: false }),
  useMarkInboxRead: () => ({ mutate: vi.fn(), isPending: false }),
}))

import { AutomatrixSection } from '@/components/matrix/automatrix-section'

/** The prose-safe wrapping rule any proactive text node must carry so long
 *  URLs/identifiers/code-like strings break instead of overflowing at 320px. */
function assertProseSafe(el: HTMLElement | null) {
  expect(el).not.toBeNull()
  expect(el!.className).toContain('[overflow-wrap:anywhere]')
}

describe('AUTO-01 proactive text readability', () => {
  beforeEach(() => {
    renderWithIntl(<AutomatrixSection />)
    fireEvent.click(screen.getByRole('button', { name: /review/i }))
  })

  it('keeps the action icon beside the label in the Astryx button slots', () => {
    const review = screen.getByRole('button', { name: /review/i })
    const icon = review.querySelector('svg')
    const label = screen.getByText('Review')

    expect(icon).not.toBeNull()
    expect(icon!.parentElement).not.toBe(label)
    expect(icon!.parentElement?.parentElement).toBe(label.parentElement)
  })

  it('wraps queue opportunity summary and rationale (14.1/14.2)', async () => {
    const summary = await screen.findByText(queueItem.summary)
    assertProseSafe(summary)
    assertProseSafe(screen.getByText(queueItem.rationale))
  })

  it('wraps inbox opportunity and result summaries (14.1/14.2)', async () => {
    fireEvent.click(screen.getByRole('button', { name: /done/i }))
    const opp = await screen.findByText(inboxItem.opportunity_summary)
    assertProseSafe(opp)
    assertProseSafe(screen.getByText(inboxItem.result_summary))
  })

  it('scrolls the panel vertically, not the underlying page (14.3)', async () => {
    const summary = await screen.findByText(queueItem.summary)
    // The nearest scroll region up the tree carries the panel-scoped overflow-y-auto.
    const scroller = summary.closest('.overflow-y-auto')
    expect(scroller).not.toBeNull()
    expect(scroller!.className).toMatch(/max-h-\[55vh\]/)
  })
})
