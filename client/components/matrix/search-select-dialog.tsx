'use client'

/**
 * SearchSelectDialog — a searchable single-select dialog.
 *
 * Adapted from Skiper UI "Skiper20" (Uniswap-style region picker). Stripped of
 * react-query / the REST countries API and re-themed to the Matrix tokens; it
 * is now fully generic and driven by the `options` prop, so it powers both the
 * model selector and the wallet network selector.
 */

import { useMemo, useState } from 'react'
import { Check, Search } from '@/lib/matrix-icons'
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { cn } from '@/lib/utils'

export interface SelectOption {
  id: string
  label: string
  description?: string
  /** Visual shown on the left (avatar, glyph, flag, etc.). */
  leading?: React.ReactNode
  /** Small meta shown on the right (before the check). */
  meta?: React.ReactNode
  disabled?: boolean
  /** Extra text included in search matching. */
  keywords?: string
}

export function SearchSelectDialog({
  open,
  onOpenChange,
  title,
  placeholder = 'Search',
  options,
  selectedId,
  onSelect,
  emptyLabel = 'Nothing found',
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  placeholder?: string
  options: SelectOption[]
  selectedId?: string | null
  onSelect: (id: string) => void
  emptyLabel?: string
}) {
  const [query, setQuery] = useState('')

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return options
    return options.filter((o) =>
      `${o.label} ${o.description ?? ''} ${o.keywords ?? ''}`.toLowerCase().includes(q),
    )
  }, [options, query])

  function choose(o: SelectOption) {
    if (o.disabled) return
    onSelect(o.id)
    onOpenChange(false)
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        onOpenChange(v)
        if (!v) setQuery('')
      }}
    >
      <DialogContent className="flex max-h-[70vh] flex-col gap-0 overflow-hidden p-0 sm:max-w-md">
        <DialogHeader className="border-border space-y-3 border-b p-4">
          <DialogTitle className="text-base">{title}</DialogTitle>
          <div className="border-border bg-muted/50 flex items-center gap-2 rounded-xl border px-3 py-2">
            <Search className="text-muted-foreground size-4 shrink-0" />
            <input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={placeholder}
              className="text-foreground placeholder:text-muted-foreground w-full bg-transparent text-sm tracking-tight outline-none"
            />
          </div>
        </DialogHeader>

        <div className="min-h-0 flex-1 overflow-y-auto p-2">
          {filtered.length === 0 ? (
            <div className="text-muted-foreground flex items-center justify-center p-8 text-sm">
              {emptyLabel}
            </div>
          ) : (
            <ul className="flex flex-col">
              {filtered.map((o) => {
                const isSelected = selectedId === o.id
                return (
                  <li key={o.id}>
                    <button
                      type="button"
                      disabled={o.disabled}
                      onClick={() => choose(o)}
                      className={cn(
                        'flex w-full items-center gap-3 rounded-xl px-3 py-2.5 text-left transition-colors',
                        o.disabled ? 'cursor-not-allowed opacity-50' : 'hover:bg-accent',
                        isSelected && 'bg-accent',
                      )}
                    >
                      {o.leading && <span className="shrink-0">{o.leading}</span>}
                      <span className="min-w-0 flex-1">
                        <span className="text-foreground block truncate text-sm font-medium">
                          {o.label}
                        </span>
                        {o.description && (
                          <span className="text-muted-foreground block truncate text-xs">
                            {o.description}
                          </span>
                        )}
                      </span>
                      {o.meta && (
                        <span className="text-muted-foreground shrink-0 text-xs">{o.meta}</span>
                      )}
                      {isSelected && <Check className="text-primary size-4 shrink-0" />}
                    </button>
                  </li>
                )
              })}
            </ul>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
