import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { Select, SelectTrigger, SelectValue } from '@/components/ui/select'

describe('tone-based input chrome', () => {
  it('keeps shared text-entry controls free of border and focus-ring chrome', () => {
    render(
      <>
        <Input aria-label="Name" />
        <Textarea aria-label="Notes" />
        <Select>
          <SelectTrigger aria-label="Choice">
            <SelectValue placeholder="Choose" />
          </SelectTrigger>
        </Select>
      </>,
    )

    const input = screen.getByRole('textbox', { name: 'Name' })
    const textarea = screen.getByRole('textbox', { name: 'Notes' })
    const select = screen.getByRole('combobox', { name: 'Choice' })

    for (const control of [input, textarea, select]) {
      expect(
        control.className
          .split(/\s+/)
          .some((token) => token === 'border' || token.startsWith('border-')),
      ).toBe(false)
      expect(control.className).not.toMatch(/focus-visible:ring/)
    }

    for (const control of [input, textarea]) {
      const tone = control.parentElement
      expect(tone).not.toBeNull()
      expect(tone).toHaveClass('bg-muted/60', 'focus-within:bg-muted')
      expect(tone).toHaveStyle({
        borderWidth: '0px',
        borderStyle: 'none',
        boxShadow: 'none',
      })
    }
    expect(select.className).toMatch(/focus-visible:bg-muted/)
  })
})
