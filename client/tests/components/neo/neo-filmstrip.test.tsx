import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen, fireEvent, waitFor } from '@testing-library/react'
import { renderWithIntl } from '@/tests/test-utils'
import type { TraceEvent } from '@/lib/api/conversations'

/* -------------------------------------------------------------------------- */
/*  BROWSER-FILMSTRIP — the client renders the filmstrip in the browser frame */
/* -------------------------------------------------------------------------- */
/*  Pins spec/browser-filmstrip req.5 + req.6 on the REAL client code paths:  */
/*  the real trace reducer (buildTaskFromTrace → parseToolStep → upsertStep), */
/*  the real NeoComputer screen builder (one screen per browser step, tab     */
/*  strip, Live pill), and the real BrowserView still render. The ONE mocked  */
/*  boundary is the authed media transport (loadMediaObjectURL) — the network */
/*  edge — so the tests assert stills load THROUGH the authed loader (req.5.1)*/
/*  instead of an unauthed <img src="/media/…">.                              */

const loadMediaObjectURL = vi.fn(async (ref: string) => `blob:authed:${ref}`)
vi.mock('@/lib/api/media', () => ({
  loadMediaObjectURL: (ref: string) => loadMediaObjectURL(ref),
}))

import { buildTaskFromTrace } from '@/hooks/api/useChat'
import { NeoComputer } from '@/components/matrix/neo/neo-computer'
import { NeoWorkspace } from '@/components/matrix/neo/neo-workspace'

/** A browser tool.step trace frame exactly as the server persists it
 *  (surfaceTool → traceWorkspaceTypes → handleTrace). */
function browserStepEvent(
  seq: number,
  id: string,
  url: string,
  title: string,
  shot: string,
  ts: string,
): TraceEvent {
  return {
    seq,
    ts,
    phase: 'act',
    type: 'tool.step',
    fields: {
      id,
      tool: 'browser__browser_navigate',
      surface: 'browser',
      running: false,
      ok: true,
      title: 'Browsing',
      action: 'navigate',
      url,
      page_title: title,
      screenshot_url: shot,
    },
  } as TraceEvent
}

const TRACE: TraceEvent[] = [
  browserStepEvent(
    1,
    'b1',
    'https://example.com',
    'Example Domain',
    '/media/shot1.jpg',
    '2026-07-11T10:00:01Z',
  ),
  browserStepEvent(
    2,
    'b2',
    'https://go.dev',
    'The Go Programming Language',
    '/media/shot2.jpg',
    '2026-07-11T10:00:05Z',
  ),
  browserStepEvent(
    3,
    'b3',
    'https://paxeer.app',
    'Paxeer',
    '/media/shot3.jpg',
    '2026-07-11T10:00:09Z',
  ),
]

beforeEach(() => {
  loadMediaObjectURL.mockClear()
})

describe('BROWSER-FILMSTRIP — reducer (req.5.2)', () => {
  it('accumulates distinct browser steps as siblings (not upsert-overwrite) with screenshot_url + ts', () => {
    const task = buildTaskFromTrace(TRACE, 'neo_run', 'browse the docs')
    const browserSteps = task.steps.filter((s) => s.kind === 'browser')
    expect(browserSteps.map((s) => s.id)).toEqual(['b1', 'b2', 'b3'])
    expect(browserSteps.map((s) => s.screenshotUrl)).toEqual([
      '/media/shot1.jpg',
      '/media/shot2.jpg',
      '/media/shot3.jpg',
    ])
    expect(browserSteps[0].ts).toBe('2026-07-11T10:00:01Z')
  })

  it('upserts the SAME id in place (running→done fills one viewport)', () => {
    const running = {
      ...browserStepEvent(1, 'b1', 'https://example.com', '', '', '2026-07-11T10:00:00Z'),
      fields: { id: 'b1', surface: 'browser', running: true, ok: true, title: 'Browsing' },
    } as TraceEvent
    const task = buildTaskFromTrace([running, TRACE[0]], 'neo_run')
    expect(task.steps.filter((s) => s.kind === 'browser')).toHaveLength(1)
    expect(task.steps[0].screenshotUrl).toBe('/media/shot1.jpg')
  })
})

describe("BROWSER-FILMSTRIP — Neo's Computer filmstrip (req.5 + req.6)", () => {
  function renderComputer() {
    const task = buildTaskFromTrace(TRACE, 'neo_run', 'browse the docs')
    return renderWithIntl(
      <NeoComputer task={task} phase="working" reduce showMedia={false} legacyOnly />,
    )
  }

  it('renders each browsed page as its own scrubbable screen, in order', () => {
    renderComputer()
    // The tab strip lists one screen per navigation, labeled by host, in order.
    const tabs = screen.getAllByRole('button', { name: /example\.com|go\.dev|paxeer\.app/ })
    expect(tabs.map((t) => t.textContent)).toEqual(['example.com', 'go.dev', 'paxeer.app'])
  })

  it('loads the still via the AUTHED media loader and labels it truthfully', async () => {
    renderComputer()
    // Newest screen (paxeer.app) is live-followed; its still must be requested
    // through the authed loader (never an unauthed /media <img>).
    await waitFor(() => expect(loadMediaObjectURL).toHaveBeenCalledWith('/media/shot3.jpg'))
    await waitFor(() => {
      const img = screen.getByAltText('Paxeer') as HTMLImageElement
      expect(img.src).toContain('blob:authed:/media/shot3.jpg')
    })
    // Truthful frame label — a capture at its timestamp, not a live-video claim.
    expect(screen.getByText(/Neo viewed ·/)).toBeTruthy()
    expect(screen.queryByText(/live video/i)).toBeNull()
  })

  it('scrubs to an earlier page via the tab strip and offers the Live pill back', async () => {
    renderComputer()
    fireEvent.click(screen.getByRole('button', { name: 'example.com' }))
    await waitFor(() => expect(loadMediaObjectURL).toHaveBeenCalledWith('/media/shot1.jpg'))
    await waitFor(() => {
      const img = screen.getByAltText('Example Domain') as HTMLImageElement
      expect(img.src).toContain('blob:authed:/media/shot1.jpg')
    })
    // Scrubbing away from the newest screen surfaces the follow-newest pill
    // (distinct from BrowserView's Shot/Live viewport toggle, which has the
    // bare accessible name "Live").
    const live = screen.getByTitle('Follow what Neo is doing now')
    fireEvent.click(live)
    await waitFor(() => {
      expect(screen.getByAltText('Paxeer')).toBeTruthy()
    })
  })

  it('renders the filmstrip history strip in order and click-to-scrubs (req.6)', async () => {
    renderComputer()
    const strip = await screen.findByTestId('filmstrip-strip')
    const thumbs = Array.from(strip.querySelectorAll('button'))
    expect(thumbs).toHaveLength(3)
    // Thumbnails load authed, in capture order.
    await waitFor(() => {
      expect(loadMediaObjectURL).toHaveBeenCalledWith('/media/shot1.jpg')
      expect(loadMediaObjectURL).toHaveBeenCalledWith('/media/shot2.jpg')
      expect(loadMediaObjectURL).toHaveBeenCalledWith('/media/shot3.jpg')
    })
    // Selecting a prior still shows THAT still in the viewport.
    fireEvent.click(thumbs[0])
    await waitFor(() => {
      const img = screen.getByAltText('Example Domain') as HTMLImageElement
      expect(img.src).toContain('blob:authed:/media/shot1.jpg')
    })
  })
})

describe('BROWSER-FILMSTRIP — BrowserView still (req.5.1)', () => {
  it('never renders an unauthed /media <img> and revokes the object URL on unmount', async () => {
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const step = buildTaskFromTrace([TRACE[0]], 'neo_run').steps[0]
    const { unmount, container } = renderWithIntl(<NeoWorkspace steps={[step]} />)
    await waitFor(() => expect(loadMediaObjectURL).toHaveBeenCalledWith('/media/shot1.jpg'))
    await waitFor(() => {
      const imgs = Array.from(container.querySelectorAll('img'))
      const still = imgs.find((i) => i.getAttribute('alt') === 'Example Domain')
      expect(still?.getAttribute('src')).toContain('blob:authed:')
    })
    // No <img> may point straight at the auth-gated /media route.
    for (const img of Array.from(container.querySelectorAll('img'))) {
      expect(img.getAttribute('src') || '').not.toMatch(/^\/media\//)
    }
    unmount()
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('blob:authed:/media/shot1.jpg'))
    revoke.mockRestore()
  })
})
