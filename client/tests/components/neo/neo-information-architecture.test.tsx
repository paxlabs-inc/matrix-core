import { describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import { NeoChatSearch } from '@/components/matrix/neo/neo-chat-search'
import { MemoryRow } from '@/components/matrix/neo/neo-timeline'
import { recentActiveConversations, type ConversationSummary } from '@/lib/api/conversations'
import type { MemoryEntry } from '@/lib/api/memory'

function conversation(index: number, archived = false): ConversationSummary {
  return {
    conversation_id: `task-${index}`,
    title: `Task ${index}`,
    preview: `Preview ${index}`,
    turn_count: index,
    updated: `2026-07-${String(28 - index).padStart(2, '0')}T12:00:00Z`,
    archived,
  }
}

describe('Neo task and memory information architecture', () => {
  it('keeps the compact sidebar to three active tasks', () => {
    const conversations = [
      conversation(1),
      conversation(2, true),
      conversation(3),
      conversation(4),
      conversation(5),
      conversation(6),
      conversation(7),
    ]

    expect(recentActiveConversations(conversations).map((item) => item.conversation_id)).toEqual([
      'task-1',
      'task-3',
      'task-4',
    ])
  })

  it('keeps all task management actions available from All tasks', () => {
    const archived: Array<[string, boolean]> = []
    const renamed: Array<[string, string]> = []
    const deleted: string[] = []

    render(
      <NeoChatSearch
        open
        onOpenChange={() => undefined}
        conversations={[conversation(1), conversation(2, true)]}
        onSelect={() => undefined}
        onNewChat={() => undefined}
        onArchive={(id, next) => archived.push([id, next])}
        onRename={(id, title) => renamed.push([id, title])}
        onDelete={(id) => deleted.push(id)}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Manage Task 1' }))
    fireEvent.click(screen.getByRole('button', { name: 'Rename' }))
    const name = screen.getByRole('textbox', { name: 'Task name' })
    fireEvent.change(name, { target: { value: 'Quarterly planning' } })
    fireEvent.keyDown(name, { key: 'Enter' })
    expect(renamed).toEqual([['task-1', 'Quarterly planning']])

    fireEvent.click(screen.getByRole('button', { name: 'Manage Task 2' }))
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }))
    expect(archived).toEqual([['task-2', false]])

    fireEvent.click(screen.getByRole('button', { name: 'Manage Task 1' }))
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    fireEvent.click(screen.getByRole('button', { name: 'Delete task' }))
    expect(deleted).toEqual(['task-1'])
  })

  it('renders a complete memory row with content and tags in the main flow', () => {
    const memory: MemoryEntry = {
      uri: 'cortex://preference/reading@1',
      type: 'Preference',
      version: 1,
      updated_at: '2026-07-27T12:00:00Z',
      form_medium: 'Prefers complete search results with enough room to scan.',
      tags: ['interface', 'search'],
    }

    render(
      <ul>
        <MemoryRow
          memory={memory}
          editable={false}
          onEdit={() => undefined}
          onDelete={() => undefined}
        />
      </ul>,
    )

    const row = screen.getByText(memory.form_medium ?? '').closest('[data-memory-row]')
    expect(row).toHaveAttribute('data-memory-row', memory.uri)
    expect(screen.getByText('interface')).toBeVisible()
    expect(screen.getByText('search')).toBeVisible()
  })
})
