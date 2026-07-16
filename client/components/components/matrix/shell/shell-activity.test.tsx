import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Shell } from '@/components/matrix/shell/shell'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE, CONSTRUCT_SURFACE_PATCH } from '@/lib/construct/store'
import type { Surface } from '@/lib/construct/types.gen'

/* -------------------------------------------------------------------------- */
/*  Activity region — live Timeline via the reused renderer (R6.2, R16.4,     */
/*  R17.2)                                                                     */
/* -------------------------------------------------------------------------- */
/*  The `activity` region hosts the live Timeline of Neo's activity and draws  */
/*  it through the EXISTING frozen `TimelineView` (dispatched by              */
/*  `SurfaceRenderer`) untouched. A `construct.surface.patch` to that Timeline */
/*  updates the surface in place — it must NOT remount the region or re-lay-   */
/*  out the rest of the shell. These tests pin those behaviours against the    */
/*  REAL Shell + the REAL placement/grouping/reducer model.                    */

const CHAT_TESTID = 'chat-thread'

function chatNode() {
  return <div data-testid={CHAT_TESTID}>chat thread</div>
}

/** A workspace whose `activity` region holds a live Timeline with one step. */
function workspaceWithTimeline(): SurfaceWorkspace {
  const timeline: Surface = {
    kind: 'timeline',
    id: 'activity-1',
    timeline: {
      title: 'Booking your trip',
      steps: [{ id: 's1', label: 'Searching flights', status: 'running' }],
    },
  }
  return applySurfaceEvent(emptyWorkspace('conv-activity'), CONSTRUCT_SURFACE, timeline, 1)
}

describe('Shell — activity region renders the live Timeline via the reused renderer', () => {
  it('places a Timeline in the activity region and renders it through TimelineView (R6.2, R17.2)', () => {
    render(<Shell workspace={workspaceWithTimeline()} narration={chatNode()} />)

    const stage = document.querySelector('[data-environment-stage]')
    const activity = document.querySelector('[data-region="activity"]')

    expect(activity).not.toBeNull()
    // The activity region sits on the environment stage (not inside narration).
    expect(stage!.contains(activity!)).toBe(true)
    // The step text comes straight from the reused TimelineView renderer.
    expect(screen.getByText('Searching flights')).toBeInTheDocument()
    // Glanceable, non-jargon region label (R12).
    expect(activity!.getAttribute('aria-label')).toBe("Neo's activity")
  })

  it('updates the Timeline in place on a patch without remounting the region or re-laying-out the shell (R16.4)', () => {
    const ws = workspaceWithTimeline()
    const { rerender } = render(<Shell workspace={ws} narration={chatNode()} />)

    // Capture stable node identities BEFORE the patch.
    const activityBefore = document.querySelector('[data-region="activity"]')
    const narrationBefore = document.querySelector('[data-region="narration"]')
    const chatBefore = screen.getByTestId(CHAT_TESTID)
    expect(activityBefore).not.toBeNull()

    // A patch that upserts a new step + flips the first to done (Timeline
    // upsert-by-step-id semantics).
    const patch: Surface = {
      kind: 'timeline',
      id: 'activity-1',
      timeline: {
        title: 'Booking your trip',
        steps: [
          { id: 's1', label: 'Searching flights', status: 'done' },
          { id: 's2', label: 'Comparing prices', status: 'running' },
        ],
      },
    }
    const next = applySurfaceEvent(ws, CONSTRUCT_SURFACE_PATCH, patch, 2)
    rerender(<Shell workspace={next} narration={chatNode()} />)

    const activityAfter = document.querySelector('[data-region="activity"]')
    const narrationAfter = document.querySelector('[data-region="narration"]')
    const chatAfter = screen.getByTestId(CHAT_TESTID)

    // The patched content is present...
    expect(screen.getByText('Comparing prices')).toBeInTheDocument()
    // ...and the region + the rest of the shell were updated IN PLACE: the same
    // DOM nodes persist (React reused them via stable keys), proving no remount
    // and no full re-layout of the environment.
    expect(activityAfter).toBe(activityBefore)
    expect(narrationAfter).toBe(narrationBefore)
    expect(chatAfter).toBe(chatBefore)
  })
})
