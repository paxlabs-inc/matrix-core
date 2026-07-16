/**
 * NEO-WORKBENCH task 3.2 (req 5.1, 4.1): the Diff view renders a REAL
 * workspace change from the daemon diff endpoint with correct per-file
 * stats, and the exec client round-trips a real command result shape.
 * The network boundary serves the exact bodies the Go handlers emit
 * (proven in neo/internal/server/workspace_test.go).
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'

// A REAL `git diff --no-color` body (the daemon returns it verbatim).
const REAL_DIFF = `diff --git a/a.txt b/a.txt
index 3367afd..3cc58df 100644
--- a/a.txt
+++ b/a.txt
@@ -1 +1,2 @@
-old
+new
+extra line
`

// jsdom has no canvas 2D context; @git-diff-view measures text with one.
beforeEach(() => {
  HTMLCanvasElement.prototype.getContext = vi.fn(
    () => ({ font: '', measureText: (s: string) => ({ width: s.length * 7 }) }) as never,
  )
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), 'http://daemon')
      const json = (body: unknown) =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        })
      if (url.pathname === '/workspace/diff') {
        return json({ git: true, diff: REAL_DIFF, untracked: ['fresh.txt'] })
      }
      if (url.pathname === '/workspace/exec' && init?.method === 'POST') {
        const body = JSON.parse(String(init.body)) as { cmd: string }
        return json({
          cmd: body.cmd,
          exit: 3,
          output: 'hello from the project\n',
          timed_out: false,
        })
      }
      return json({})
    }),
  )
})

import { CodyDiffView } from '@/components/matrix/cody/cody-diff'
import { execCommand } from '@/lib/api/workspace'

describe('Diff view — fed by the daemon diff endpoint', () => {
  it('renders the changed file with its +/- stats and the untracked file', async () => {
    renderWithIntl(<CodyDiffView projectID="app" />)
    await waitFor(() => expect(screen.getByText('a.txt')).toBeInTheDocument())
    // Per-file stats from the REAL hunk: +2 / -1.
    expect(screen.getByText('+2')).toBeInTheDocument()
    expect(screen.getByText('-1')).toBeInTheDocument()
    // 2 changed = the diffed file + the untracked one.
    expect(screen.getByText(/2 changed/)).toBeInTheDocument()
    expect(screen.getByText('fresh.txt')).toBeInTheDocument()
  })
})

describe('exec — the terminal panel round-trip contract', () => {
  it('POSTs the command with the project scope and folds the honest result', async () => {
    const res = await execCommand('app', 'cat a.txt; exit 3')
    expect(res.exit).toBe(3)
    expect(res.timed_out).toBe(false)
    expect(res.output).toContain('hello from the project')
    const call = (global.fetch as ReturnType<typeof vi.fn>).mock.calls.find(([u]) =>
      String(u).includes('/workspace/exec'),
    )!
    expect(String(call[0])).toContain('project=app')
  })
})
