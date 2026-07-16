import { describe, it, expect, vi } from 'vitest'
import { screen } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'
import type { NeoTask } from '@/hooks/api/useChat'

/* -------------------------------------------------------------------------- */
/*  F2 — NeoSurface renders the visible "reconnecting" / "retrying" copy      */
/* -------------------------------------------------------------------------- */
/*  Pins NEO-UX-FIXES.kvx [item.F2].acceptance for the surface side: when the */
/*  hook reports `resuming=true` the surface renders plain-language reconnect */
/*  copy (no "SSE", no "broker", no protocol jargon — ux_truth). The visible  */
/*  string is asserted via case-insensitive search for "Reconnect".           */

vi.mock('sonner', () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
  Toaster: () => null,
}))

vi.mock('@/lib/api/media', () => ({
  uploadMedia: vi.fn(),
  mediaKindForMime: () => 'file',
}))

import { NeoSurface } from '@/components/matrix/neo/neo-surface'

function seededTask(): NeoTask {
  return {
    intentId: 'neo_inflight',
    title: 'do something long',
    steps: [
      {
        id: 'seed',
        kind: 'narration',
        running: true,
        ok: true,
        title: '',
        text: 'Working on your request',
      },
    ],
    searches: [],
    media: [],
    artifacts: [],
    surfaces: [],
    answer: '',
    done: false,
  }
}

describe('neo-surface — F2 visible resuming / retrying copy', () => {
  it('neo-surface.rendersWorkingBannerOnResume — visible string contains "Reconnect" when resuming=true', () => {
    renderWithIntl(
      <NeoSurface
        phase="working"
        task={seededTask()}
        messages={[{ id: 'u1', role: 'user', text: 'do something long', ts: 1719196800000 }]}
        send={() => {}}
        pendingGate={null}
        resuming
        connectionRetrying={false}
        answerGate={() => {}}
        dismissTask={() => {}}
      />,
    )

    // The StatePill (and ComputerChip if rendered) carry the plain-language
    // resume copy. Case-insensitive match on the literal substring per spec.
    const matches = screen.getAllByText(/reconnect/i)
    expect(matches.length).toBeGreaterThan(0)
    // ux_truth: no protocol jargon must appear on the visible surface.
    expect(screen.queryByText(/SSE|broker|topic|MCL|cortex|Cassandra|Merkle/i)).toBeNull()
  })

  it('neo-surface — connectionRetrying renders plain-language "Connection lost — retrying…"', () => {
    renderWithIntl(
      <NeoSurface
        phase="working"
        task={seededTask()}
        messages={[{ id: 'u1', role: 'user', text: 'do something long', ts: 1719196800000 }]}
        send={() => {}}
        pendingGate={null}
        resuming={false}
        connectionRetrying
        answerGate={() => {}}
        dismissTask={() => {}}
      />,
    )

    const matches = screen.getAllByText(/connection lost/i)
    expect(matches.length).toBeGreaterThan(0)
    // The visible copy includes the retrying signal.
    expect(matches[0].textContent?.toLowerCase()).toContain('retrying')
  })
})
