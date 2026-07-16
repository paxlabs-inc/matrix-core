/**
 * NEO-WORKBENCH task 4.2 (req 7.2, 7.3): the Preview pane tracks a REAL
 * preview event sequence through all four states with bolt's chrome and
 * honest empty/error states — never a stale frame presented as live.
 */
import { describe, it, expect, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'

import { foldPreviewEvent, type NeoPreview } from '@/hooks/api/useChat'
import { WorkbenchPreview } from '@/components/matrix/cody/workbench-preview'

describe('Preview pane — the real lifecycle, honestly rendered', () => {
  it('tracks pending → ready → failed → expired from folded events', () => {
    // The exact durable sequence the daemon controller emits.
    const sequence: [string, Record<string, unknown> | undefined][] = [
      ['preview.pending', undefined],
      ['preview.ready', { url: 'https://sbx.example/app/' }],
      ['preview.failed', { reason: 'The dev server did not come up.' }],
      ['preview.expired', undefined],
    ]
    let preview: NeoPreview | undefined
    const onRequestPreview = vi.fn()
    const r = render(<WorkbenchPreview preview={preview} onRequestPreview={onRequestPreview} />)

    // No preview yet: honest empty state with a start action.
    expect(screen.getByTestId('preview-empty')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /start preview/i }))
    expect(onRequestPreview).toHaveBeenCalledTimes(1)

    // pending: a real progress state, no frame.
    preview = foldPreviewEvent(...sequence[0])!
    r.rerender(<WorkbenchPreview preview={preview} onRequestPreview={onRequestPreview} />)
    expect(screen.queryByTestId('preview-frame')).not.toBeInTheDocument()
    expect(screen.getByText(/starting the preview sandbox/i)).toBeInTheDocument()

    // ready: iframe on the sandbox URL with the chrome (address path, reload,
    // open-in-new-tab).
    preview = foldPreviewEvent(...sequence[1])!
    r.rerender(<WorkbenchPreview preview={preview} onRequestPreview={onRequestPreview} />)
    const frame = screen.getByTestId('preview-frame') as HTMLIFrameElement
    expect(frame.src).toContain('sbx.example')
    const pathBar = screen.getByLabelText('Preview path') as HTMLInputElement
    fireEvent.change(pathBar, { target: { value: '/about' } })
    fireEvent.keyDown(pathBar, { key: 'Enter' })
    expect((screen.getByTestId('preview-frame') as HTMLIFrameElement).src).toContain('/about')
    expect(screen.getByLabelText('Open preview in a new tab')).toHaveAttribute(
      'href',
      expect.stringContaining('sbx.example'),
    )
    expect(screen.getByLabelText('Reload preview')).toBeInTheDocument()

    // failed: the frame is GONE (never a stale frame), the reason shows.
    preview = foldPreviewEvent(...sequence[2])!
    r.rerender(<WorkbenchPreview preview={preview} onRequestPreview={onRequestPreview} />)
    expect(screen.queryByTestId('preview-frame')).not.toBeInTheDocument()
    expect(screen.getByText(/did not come up/i)).toBeInTheDocument()

    // expired: honest teardown state with a restart action.
    preview = foldPreviewEvent(...sequence[3])!
    r.rerender(<WorkbenchPreview preview={preview} onRequestPreview={onRequestPreview} />)
    expect(screen.queryByTestId('preview-frame')).not.toBeInTheDocument()
    expect(screen.getByText(/preview expired/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /start it again/i }))
    expect(onRequestPreview).toHaveBeenCalledTimes(2)
  })
})
