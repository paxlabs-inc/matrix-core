import { describe, expect, it } from 'vitest'
import { fireEvent, screen } from '@testing-library/react'
import { SettingsSheet } from '@/components/matrix/settings-sheet'
import { QueryProvider } from '@/lib/query/QueryProvider'
import { AuthProvider } from '@/lib/auth/AuthProvider'
import { renderWithIntl } from '@/tests/test-utils'

function renderSettings() {
  return renderWithIntl(
    <QueryProvider>
      <AuthProvider>
        <SettingsSheet open onOpenChange={() => undefined} />
      </AuthProvider>
    </QueryProvider>,
  )
}

describe('SettingsSheet', () => {
  it('presents the complete categorized settings navigation', () => {
    renderSettings()

    const tabs = screen.getAllByRole('button')
    expect(tabs.map((tab) => tab.textContent)).toEqual(
      expect.arrayContaining([
        expect.stringContaining('Account'),
        expect.stringContaining('Preferences'),
        expect.stringContaining('Personalization'),
        expect.stringContaining('Memory'),
        expect.stringContaining('Notifications'),
        expect.stringContaining('Computer'),
        expect.stringContaining('Configuration'),
        expect.stringContaining('Connectors'),
        expect.stringContaining('Skills'),
        expect.stringContaining('Credential vault'),
        expect.stringContaining('Other'),
      ]),
    )
  })

  it('opens a category as a focused subpage while preserving real controls', () => {
    renderSettings()

    fireEvent.click(screen.getByRole('button', { name: /notifications/i }))

    expect(screen.getByRole('switch', { name: 'Completed tasks' })).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'Needs your input' })).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: 'Failed tasks' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Back to settings' })).toBeInTheDocument()
  })

  it('owns Self-Model inside the Computer settings category', () => {
    renderWithIntl(
      <QueryProvider>
        <AuthProvider>
          <SettingsSheet open onOpenChange={() => undefined} onOpenSelfModel={() => undefined} />
        </AuthProvider>
      </QueryProvider>,
    )

    fireEvent.click(screen.getByRole('button', { name: /computer/i }))

    expect(screen.getByRole('button', { name: /Self-Model/i })).toBeInTheDocument()
  })
})
