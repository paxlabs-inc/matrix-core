import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Shell } from '@/components/matrix/shell/shell'
import { applySurfaceEvent, emptyWorkspace, type SurfaceWorkspace } from '@/lib/construct/workspace'
import { CONSTRUCT_SURFACE } from '@/lib/construct/store'
import type { Surface } from '@/lib/construct/types.gen'

/* -------------------------------------------------------------------------- */
/*  Frame inversion (R3.1, R3.2, R3.4, R3.5, R17.5)                           */
/* -------------------------------------------------------------------------- */
/*  The shell mounts the ENVIRONMENT as the route root and hosts the chat as  */
/*  exactly ONE panel occupying the `narration` region within it — never as   */
/*  the page that contains the environment. These tests pin those structural  */
/*  invariants against the REAL `Shell` + the REAL placement/grouping model;   */
/*  the chat thread is represented by a sentinel node (the production chat is  */
/*  mounted as this same slot in the dashboard).                              */

const CHAT_TESTID = 'chat-thread'

function chatNode() {
  return <div data-testid={CHAT_TESTID}>chat thread</div>
}

/** A workspace with one non-narration surface placed (a Timeline → `activity`). */
function workspaceWithActivity(): SurfaceWorkspace {
  const timeline: Surface = {
    kind: 'timeline',
    id: 'activity-1',
    timeline: { title: 'Activity', steps: [] },
  }
  return applySurfaceEvent(emptyWorkspace('conv-shell'), CONSTRUCT_SURFACE, timeline, 1)
}

describe('Shell — frame inversion', () => {
  it('renders the environment as the route root with chat as a nested narration region (R3.1, R3.2)', () => {
    render(<Shell workspace={emptyWorkspace('conv-shell')} narration={chatNode()} />)

    const root = document.querySelector('[data-environment-root]')
    const narration = document.querySelector('[data-region="narration"]')
    const chat = screen.getByTestId(CHAT_TESTID)

    expect(root).not.toBeNull()
    expect(narration).not.toBeNull()
    // The narration region is a DESCENDANT of the environment root — the
    // environment contains chat, not the other way around (R3.4).
    expect(root!.contains(narration!)).toBe(true)
    expect(narration!.contains(chat)).toBe(true)
    // And chat is never an ancestor of the environment root (no chat-as-page).
    expect(chat.contains(root!)).toBe(false)
  })

  it('keeps the environment as root and chat contained when NO narration surface is present (R3.5)', () => {
    // emptyWorkspace has zero surfaces of any kind, including `narration`.
    const ws = emptyWorkspace('conv-shell')
    expect(ws.surfaces.size).toBe(0)

    render(<Shell workspace={ws} narration={chatNode()} />)

    const root = document.querySelector('[data-environment-root]')
    const narration = document.querySelector('[data-region="narration"]')

    // The environment is still the root; chat did not become the containing page.
    expect(root).not.toBeNull()
    expect(narration).not.toBeNull()
    expect(root!.contains(narration!)).toBe(true)
    // With nothing else placed, the environment stage is absent — but the
    // environment root and its narration panel remain.
    expect(document.querySelector('[data-environment-stage]')).toBeNull()
    expect(screen.getByTestId(CHAT_TESTID)).toBeInTheDocument()
  })

  it('treats narration as exactly one of the regions: the stage holds the OTHER regions beside it (R3.4)', () => {
    render(<Shell workspace={workspaceWithActivity()} narration={chatNode()} />)

    const root = document.querySelector('[data-environment-root]')
    const narration = document.querySelector('[data-region="narration"]')
    const stage = document.querySelector('[data-environment-stage]')
    const activity = document.querySelector('[data-region="activity"]')

    expect(root).not.toBeNull()
    // Both the narration panel and the environment stage are siblings under the
    // single environment root — neither region contains the other (R3.4).
    expect(root!.contains(narration!)).toBe(true)
    expect(root!.contains(stage!)).toBe(true)
    expect(narration!.contains(stage!)).toBe(false)
    expect(stage!.contains(narration!)).toBe(false)
    // The placed Timeline landed in the `activity` region on the stage.
    expect(activity).not.toBeNull()
    expect(stage!.contains(activity!)).toBe(true)
    // Chat is still present as the narration panel's content.
    expect(narration!.contains(screen.getByTestId(CHAT_TESTID))).toBe(true)
  })
})
