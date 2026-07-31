import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Shell } from '@/components/matrix/shell/shell'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE } from '@/lib/construct/store'
import type { Surface } from '@/lib/construct/types.gen'

/* -------------------------------------------------------------------------- */
/*  The stage opens on WORK, not on mount (FINANCE req 7.2 / 7.3)             */
/* -------------------------------------------------------------------------- */
/*  Neo's Computer is always mounted so the user can reach its power controls  */
/*  at any moment — but the app must not open onto a stage standing open with  */
/*  nothing in it. These pin the real Shell's behaviour: closed on a cold      */
/*  open, opening itself when work arrives, an explicit close that sticks      */
/*  while that run continues, and a reopen control that is always reachable.   */

const CHAT = 'chat-thread'
const COMPUTER = 'neo-computer'

const chatNode = () => <div data-testid={CHAT}>chat thread</div>
const computerNode = () => <div data-testid={COMPUTER}>Neo&apos;s Computer</div>

function workspaceWithActivity(): SurfaceWorkspace {
  const timeline: Surface = {
    kind: 'timeline',
    id: 'activity-1',
    timeline: { title: 'Activity', steps: [] },
  }
  return applySurfaceEvent(emptyWorkspace('conv-shell'), CONSTRUCT_SURFACE, timeline, 1)
}

/** The stage is "closed" on desktop when it carries the closed marker. */
function stageClosed(): boolean {
  const stage = document.querySelector('[data-environment-stage]')
  if (!stage) return true
  return stage.hasAttribute('data-desktop-closed')
}

describe('Shell — the stage opens on work, not on mount', () => {
  it('starts CLOSED when the Computer is mounted but holds nothing', () => {
    render(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive={false}
      />,
    )

    // The stage exists (the Computer is mounted and reachable) but it is not
    // standing open on a cold entry to the app.
    expect(document.querySelector('[data-environment-stage]')).not.toBeNull()
    expect(stageClosed()).toBe(true)
    // Chat gets the full width instead of sitting beside an empty viewport.
    expect(screen.getByTestId(CHAT)).toBeInTheDocument()
  })

  it('offers a reopen control at all times so the user can open the Computer whenever they want', async () => {
    render(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive={false}
      />,
    )

    const reopen = screen.getByRole('button', { name: /Open Neo's work/i })
    expect(reopen).toBeInTheDocument()

    await userEvent.click(reopen)
    expect(stageClosed()).toBe(false)
  })

  it('keeps the real stage content mounted while its panel closes and reopens', async () => {
    render(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive
      />,
    )

    const stage = document.querySelector('[data-environment-stage]')
    const computer = screen.getByTestId(COMPUTER)
    expect(stage).not.toBeNull()

    await userEvent.click(screen.getByRole('button', { name: /Close Neo's work/i }))
    expect(stageClosed()).toBe(true)
    expect(document.querySelector('[data-environment-stage]')).toBe(stage)
    expect(screen.getByTestId(COMPUTER)).toBe(computer)

    await userEvent.click(screen.getByRole('button', { name: /Open Neo's work/i }))
    expect(stageClosed()).toBe(false)
    expect(document.querySelector('[data-environment-stage]')).toBe(stage)
    expect(screen.getByTestId(COMPUTER)).toBe(computer)
  })

  it('opens itself when real work arrives', () => {
    const { rerender } = render(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive={false}
      />,
    )
    expect(stageClosed()).toBe(true)

    rerender(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive
      />,
    )
    expect(stageClosed()).toBe(false)
  })

  it('keeps an explicit close closed while that run keeps producing events', async () => {
    const { rerender } = render(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive
      />,
    )
    expect(stageClosed()).toBe(false)

    await userEvent.click(screen.getByRole('button', { name: /Close Neo's work/i }))
    expect(stageClosed()).toBe(true)

    // More of the SAME run's work lands — the user's choice must hold.
    rerender(
      <Shell
        workspace={workspaceWithActivity()}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive
      />,
    )
    expect(stageClosed()).toBe(true)
  })

  it('reopens for a NEW run after the previous one went quiet', async () => {
    const { rerender } = render(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive
      />,
    )
    await userEvent.click(screen.getByRole('button', { name: /Close Neo's work/i }))
    expect(stageClosed()).toBe(true)

    // The run ends…
    rerender(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive={false}
      />,
    )
    // …and a new one starts: the stale close does not hide the new work.
    rerender(
      <Shell
        workspace={emptyWorkspace('conv-1')}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive
      />,
    )
    expect(stageClosed()).toBe(false)
  })

  it('still shows the stage for placed surfaces even with no live run', () => {
    render(
      <Shell
        workspace={workspaceWithActivity()}
        narration={chatNode()}
        environment={computerNode()}
        environmentActive={false}
      />,
    )
    // A placed surface IS something to look at, regardless of run phase.
    expect(stageClosed()).toBe(false)
    expect(document.querySelector('[data-region="activity"]')).not.toBeNull()
  })
})
