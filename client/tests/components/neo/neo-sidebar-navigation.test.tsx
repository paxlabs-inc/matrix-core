import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import { NeoSurface } from '@/components/matrix/neo/neo-surface'
import { renderWithIntl } from '@/tests/test-utils'

describe('Neo sidebar navigation', () => {
  it('links to Finance and does not expose Self-Model in the sidebar', () => {
    renderWithIntl(
      <NeoSurface
        phase="idle"
        task={null}
        messages={[]}
        send={() => undefined}
        pendingGate={null}
        resuming={false}
        connectionRetrying={false}
        answerGate={() => undefined}
        dismissTask={() => undefined}
        conversations={[]}
        onNewChat={() => undefined}
        onOpenSettings={() => undefined}
      />,
    )

    expect(
      screen
        .getAllByRole('link', { name: 'Finance' })
        .every((link) => link.getAttribute('href')?.endsWith('/finance')),
    ).toBe(true)
    expect(screen.getByRole('img', { name: 'Neo' })).toBeVisible()
    expect(screen.queryByText('Matrix')).toBeNull()
    expect(screen.queryByText('Self-Model')).toBeNull()
  })
})
