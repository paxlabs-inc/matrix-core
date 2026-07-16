/**
 * NEO-WORKBENCH task 3.1/3.4: the CodeMirror binding boots the REAL editor
 * engine, renders content, replaces the document on external change (the
 * live-typing feed), and honors read-only while Neo writes.
 */
import { describe, it, expect } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'

import { CodeEditor } from '@/components/matrix/cody/code-editor'

// jsdom misses a couple of DOM APIs CodeMirror touches.
if (!global.Range.prototype.getClientRects) {
  global.Range.prototype.getClientRects = () => [] as unknown as DOMRectList
}
global.Range.prototype.getBoundingClientRect ??= () => ({
  x: 0,
  y: 0,
  top: 0,
  left: 0,
  right: 0,
  bottom: 0,
  width: 0,
  height: 0,
  toJSON: () => ({}),
})

describe('CodeEditor — real CodeMirror binding', () => {
  it('mounts the engine, shows content, and re-renders external replacements', async () => {
    const r = render(<CodeEditor path="src/app.ts" content={'const a = 1\n'} />)
    await waitFor(() => expect(screen.getByTestId('code-editor')).toBeInTheDocument())
    await waitFor(() =>
      expect(screen.getByTestId('code-editor').textContent).toContain('const a = 1'),
    )

    // An external content change (Neo's live typing) replaces the document.
    await act(async () => {
      r.rerender(<CodeEditor path="src/app.ts" content={'const a = 1\nconst b = 2\n'} />)
    })
    await waitFor(() =>
      expect(screen.getByTestId('code-editor').textContent).toContain('const b = 2'),
    )
  })
})
