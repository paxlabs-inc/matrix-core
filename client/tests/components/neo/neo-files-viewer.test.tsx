import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import {
  FileRow,
  WorkspaceImagePreview,
  WorkspaceFileViewer,
  workspaceViewerKind,
} from '@/components/matrix/neo/neo-files'

describe('Workspace file viewer', () => {
  it('routes supported and unknown file types to honest viewer modes', () => {
    expect(workspaceViewerKind('report.md')).toBe('text')
    expect(workspaceViewerKind('main.tsx')).toBe('text')
    expect(workspaceViewerKind('README')).toBe('text')
    expect(workspaceViewerKind('chart.png')).toBe('image')
    expect(workspaceViewerKind('demo.mp4')).toBe('video')
    expect(workspaceViewerKind('voice.wav')).toBe('audio')
    expect(workspaceViewerKind('brief.pdf')).toBe('pdf')
    expect(workspaceViewerKind('archive.zip')).toBe('binary')
  })

  it('selects a file from the row while keeping download secondary', () => {
    let opened = false
    render(
      <ul>
        <FileRow
          entry={{ path: 'reports/brief.pdf', dir: false, size: 2048 }}
          showPath
          selected={false}
          onOpen={() => {
            opened = true
          }}
        />
      </ul>,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Open brief.pdf' }))

    expect(opened).toBe(true)
    expect(screen.getByRole('button', { name: 'Download brief.pdf' })).toBeInTheDocument()
  })

  it('shows an explicit fallback for an unknown binary file', () => {
    render(
      <WorkspaceFileViewer
        entry={{ path: 'build/archive.bin', dir: false, size: 512 }}
        onBack={() => undefined}
      />,
    )

    expect(screen.getByText('No inline preview for this file')).toBeVisible()
    expect(screen.getByText(/kept byte-exact/i)).toBeVisible()
    expect(screen.getByRole('button', { name: 'Download archive.bin' })).toBeInTheDocument()
  })

  it('renders authenticated image sources inside the Workspace detail pane', () => {
    render(<WorkspaceImagePreview src="blob:workspace-preview" name="chart.png" />)

    expect(screen.getByRole('img', { name: 'chart.png' })).toHaveAttribute(
      'src',
      'blob:workspace-preview',
    )
  })
})
