'use client'

import { IconArrowRight, IconCornerDownLeft } from '@tabler/icons-react'
import { useRouter } from 'next/navigation'
import { useCallback, useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Kbd } from '@/components/ui/kbd'

const navItems = [
  { href: '/', label: 'Home', keywords: ['home', 'main', 'index'] },
  {
    href: '/dashboard',
    label: 'Dashboard',
    keywords: ['dashboard', 'overview'],
  },
  {
    href: '/settings',
    label: 'Settings',
    keywords: ['settings', 'preferences'],
  },
]

const pageGroups = [
  {
    name: 'Getting Started',
    pages: [
      {
        name: 'Introduction',
        href: '/docs/introduction',
        keywords: ['intro', 'start'],
      },
      {
        name: 'Installation',
        href: '/docs/installation',
        keywords: ['install', 'setup'],
      },
      {
        name: 'Quick Start',
        href: '/docs/quick-start',
        keywords: ['quick', 'begin'],
      },
    ],
  },
  {
    name: 'Utilities',
    pages: [
      {
        name: 'Typography',
        href: '/docs/utilities/typography',
        keywords: ['text', 'font'],
      },
      {
        name: 'Colors',
        href: '/docs/utilities/colors',
        keywords: ['color', 'theme'],
      },
      {
        name: 'Spacing',
        href: '/docs/utilities/spacing',
        keywords: ['margin', 'padding'],
      },
    ],
  },
]

const colorGroups = [
  {
    name: 'Neutral',
    colors: [
      {
        name: 'Neutral 50',
        className: 'neutral-50',
        value: 'oklch(0.985 0 0)',
      },
      {
        name: 'Neutral 100',
        className: 'neutral-100',
        value: 'oklch(0.97 0 0)',
      },
      {
        name: 'Neutral 200',
        className: 'neutral-200',
        value: 'oklch(0.922 0 0)',
      },
      {
        name: 'Neutral 500',
        className: 'neutral-500',
        value: 'oklch(0.556 0 0)',
      },
      {
        name: 'Neutral 900',
        className: 'neutral-900',
        value: 'oklch(0.205 0 0)',
      },
    ],
  },
  {
    name: 'Blue',
    colors: [
      {
        name: 'Blue 50',
        className: 'blue-50',
        value: 'oklch(0.97 0.014 254.604)',
      },
      {
        name: 'Blue 500',
        className: 'blue-500',
        value: 'oklch(0.623 0.214 259.815)',
      },
      {
        name: 'Blue 600',
        className: 'blue-600',
        value: 'oklch(0.546 0.245 262.881)',
      },
    ],
  },
]

export function CommandMenu03() {
  const router = useRouter()
  const [open, setOpen] = useState(true)

  const copyToClipboard = useCallback((text: string) => {
    navigator.clipboard.writeText(text)
  }, [])

  const runCommand = useCallback((command: () => unknown) => {
    setOpen(false)
    command()
  }, [])

  useEffect(() => {
    const down = (e: KeyboardEvent) => {
      if ((e.key === 'k' && (e.metaKey || e.ctrlKey)) || e.key === '/') {
        if (
          (e.target instanceof HTMLElement && e.target.isContentEditable) ||
          e.target instanceof HTMLInputElement ||
          e.target instanceof HTMLTextAreaElement ||
          e.target instanceof HTMLSelectElement
        ) {
          return
        }
        e.preventDefault()
        setOpen((prev) => !prev)
      }
    }

    document.addEventListener('keydown', down)
    return () => document.removeEventListener('keydown', down)
  }, [])

  return (
    <>
      <Button onClick={() => setOpen(true)} variant="outline">
        Open Command Menu
      </Button>

      <Dialog onOpenChange={setOpen} open={open}>
        <DialogContent className="rounded-xl border-none bg-clip-padding p-2 pb-11 shadow-2xl ring-4 ring-neutral-200/80 dark:bg-neutral-900 dark:ring-neutral-800">
          <DialogHeader className="sr-only">
            <DialogTitle>Search documentation...</DialogTitle>
            <DialogDescription>Search for a command to run...</DialogDescription>
          </DialogHeader>

          <Command className="**:data-[slot=command-input-wrapper]:border-input **:data-[slot=command-input-wrapper]:bg-input/50 rounded-none bg-transparent **:data-[slot=command-input]:h-9! **:data-[slot=command-input]:py-0 **:data-[slot=command-input-wrapper]:mb-0 **:data-[slot=command-input-wrapper]:h-9! **:data-[slot=command-input-wrapper]:rounded-md **:data-[slot=command-input-wrapper]:border">
            <CommandInput placeholder="Search documentation..." />
            <CommandList className="no-scrollbar min-h-80 scroll-pt-2 scroll-pb-1.5">
              <CommandEmpty className="text-muted-foreground py-12 text-center text-sm">
                No results found.
              </CommandEmpty>

              {navItems.length > 0 && (
                <CommandGroup
                  className="p-0! **:[[cmdk-group-heading]]:scroll-mt-16 **:[[cmdk-group-heading]]:p-3! **:[[cmdk-group-heading]]:pb-1!"
                  heading="Pages"
                >
                  {navItems.map((item) => (
                    <CommandItem
                      className="hover:border-input hover:bg-input/50 h-9 rounded-md border border-transparent px-3! font-medium"
                      key={item.href}
                      keywords={item.keywords}
                      onSelect={() => {
                        runCommand(() => router.push(item.href))
                      }}
                      value={`Navigation ${item.label}`}
                    >
                      <IconArrowRight aria-hidden="true" className="size-4" />
                      {item.label}
                    </CommandItem>
                  ))}
                </CommandGroup>
              )}

              {pageGroups.map((group) => (
                <CommandGroup
                  className="p-0! **:[[cmdk-group-heading]]:scroll-mt-16 **:[[cmdk-group-heading]]:p-3! **:[[cmdk-group-heading]]:pb-1!"
                  heading={group.name}
                  key={group.name}
                >
                  {group.pages.map((page) => {
                    const isComponent = page.href.includes('/components/')
                    return (
                      <CommandItem
                        className="hover:border-input hover:bg-input/50 h-9 rounded-md border border-transparent px-3! font-medium"
                        key={page.href}
                        keywords={page.keywords}
                        onSelect={() => {
                          runCommand(() => router.push(page.href))
                        }}
                        value={`${group.name} ${page.name}`}
                      >
                        {isComponent ? (
                          <div className="border-muted-foreground aspect-square size-4 rounded-full border border-dashed" />
                        ) : (
                          <IconArrowRight aria-hidden="true" className="size-4" />
                        )}
                        {page.name}
                      </CommandItem>
                    )
                  })}
                </CommandGroup>
              ))}

              {colorGroups.map((colorGroup) => (
                <CommandGroup
                  className="p-0! **:[[cmdk-group-heading]]:p-3!"
                  heading={colorGroup.name}
                  key={colorGroup.name}
                >
                  {colorGroup.colors.map((color) => (
                    <CommandItem
                      className="hover:border-input hover:bg-input/50 h-9 rounded-md border border-transparent px-3! font-medium"
                      key={color.className}
                      keywords={['color', color.name, color.className]}
                      onSelect={() => {
                        runCommand(() => copyToClipboard(color.value))
                      }}
                      value={color.className}
                    >
                      <div
                        className="aspect-square size-4 rounded-sm border"
                        style={{ backgroundColor: color.value }}
                      />
                      {color.className}
                      <span className="text-muted-foreground ml-auto font-mono text-xs font-normal tabular-nums">
                        {color.value}
                      </span>
                    </CommandItem>
                  ))}
                </CommandGroup>
              ))}
            </CommandList>
          </Command>

          <div className="text-muted-foreground absolute inset-x-0 bottom-0 z-20 flex h-10 items-center gap-2 rounded-b-xl border-t border-t-neutral-100 bg-neutral-50 px-4 text-xs font-medium dark:border-t-neutral-700 dark:bg-neutral-800">
            <Kbd>
              <IconCornerDownLeft aria-hidden="true" className="size-3" />
            </Kbd>
            Select
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
