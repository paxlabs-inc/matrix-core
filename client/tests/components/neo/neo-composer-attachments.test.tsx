import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import userEvent from '@testing-library/user-event'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { NeoComposer } from '@/components/centra/agents/neo/neo-composer'

function renderComposer(onAddFiles: (files: File[]) => void, attachments: File[] = []) {
  return render(
    <NeoComposer
      value=""
      onChange={() => undefined}
      onSubmit={() => undefined}
      mode="auto"
      onModeChange={() => undefined}
      attachments={attachments}
      onAddFiles={onAddFiles}
      onRemoveFile={() => undefined}
    />,
  )
}

function ControlledComposer() {
  const [value, setValue] = useState('')
  return (
    <NeoComposer
      value={value}
      onChange={setValue}
      onSubmit={() => undefined}
      mode="auto"
      onModeChange={() => undefined}
    />
  )
}

describe('NeoComposer attachment chooser', () => {
  it('keeps the focused textarea mounted when the composer expands', async () => {
    render(<ControlledComposer />)
    const textarea = screen.getByRole('textbox', { name: 'Ask Neo' })
    textarea.focus()

    fireEvent.change(textarea, { target: { value: 'First line\nSecond line' } })

    await waitFor(() =>
      expect(document.querySelector('[data-neo-composer]')).toHaveAttribute(
        'data-layout',
        'expanded',
      ),
    )
    expect(screen.getByRole('textbox', { name: 'Ask Neo' })).toBe(textarea)
    expect(textarea).toHaveFocus()
  })

  it('offers explicit image, video, and file choices with format guidance', async () => {
    const user = userEvent.setup()
    renderComposer(() => undefined)

    await user.click(screen.getByRole('button', { name: 'Add an attachment' }))

    expect(screen.getByRole('menuitem', { name: /ImagePhotos, screenshots/i })).toBeVisible()
    expect(screen.getByRole('menuitem', { name: /VideoVideo clips/i })).toBeVisible()
    expect(screen.getByRole('menuitem', { name: /FileDocuments, audio/i })).toBeVisible()
    expect(screen.getByText(/choose multiple items/i)).toBeVisible()
  })

  it('filters media pickers and sends every selection through the same staging callback', () => {
    const staged: File[][] = []
    renderComposer((files) => staged.push(files))

    const imageInput = screen.getByLabelText('Choose images')
    const videoInput = screen.getByLabelText('Choose videos')
    const fileInput = screen.getByLabelText('Choose files')
    const image = new File(['image'], 'chart.png', { type: 'image/png' })
    const video = new File(['video'], 'demo.mp4', { type: 'video/mp4' })
    const document = new File(['document'], 'notes.md', { type: 'text/markdown' })

    expect(imageInput).toHaveAttribute('accept', 'image/*')
    expect(videoInput).toHaveAttribute('accept', 'video/*')
    expect(fileInput).not.toHaveAttribute('accept')
    expect(imageInput).toHaveAttribute('multiple')
    expect(videoInput).toHaveAttribute('multiple')
    expect(fileInput).toHaveAttribute('multiple')

    fireEvent.change(imageInput, { target: { files: [image] } })
    fireEvent.change(videoInput, { target: { files: [video] } })
    fireEvent.change(fileInput, { target: { files: [document] } })

    expect(staged).toEqual([[image], [video], [document]])
  })
})
