import { describe, expect, it } from 'vitest'
import userEvent from '@testing-library/user-event'
import { NeoAssistantMessage } from '@/components/matrix/neo/neo-message'
import type { ChatMessage } from '@/hooks/api/useChat'
import { renderWithIntl, screen } from '@/tests/test-utils'

describe('NeoAssistantMessage incomplete delivery', () => {
  it('renders useful saved work with a plain-language resume affordance', async () => {
    const resumes: string[] = []
    const message: ChatMessage = {
      id: 'partial-1',
      role: 'assistant',
      text: 'The verified sections are saved; the final comparison remains.',
      deliveryStatus: 'honest_partial',
      resumable: true,
      ts: 1,
    }

    renderWithIntl(
      <NeoAssistantMessage message={message} onResume={() => resumes.push('resume')} />,
    )

    expect(screen.getByText('Useful work is ready, with more still to do.')).toBeVisible()
    expect(screen.getByText(message.text)).toBeVisible()
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(resumes).toEqual(['resume'])
  })

  it('labels an incomplete answer without offering an unsupported resume', () => {
    const message: ChatMessage = {
      id: 'incomplete-1',
      role: 'assistant',
      text: 'The run stopped before the final check.',
      deliveryStatus: 'incomplete',
      ts: 2,
    }

    renderWithIntl(<NeoAssistantMessage message={message} />)

    expect(screen.getByText('This needs another pass.')).toBeVisible()
    expect(screen.queryByRole('button', { name: 'Continue' })).not.toBeInTheDocument()
  })
})
