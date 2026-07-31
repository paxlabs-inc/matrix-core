'use client'

import * as React from 'react'
import { Dialog as AstryxDialog } from '@astryxdesign/core/Dialog'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Button } from '@astryxdesign/core/Button'
import { XIcon } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'

interface SheetContextValue {
  open: boolean
  setOpen(open: boolean): void
}

const SheetContext = React.createContext<SheetContextValue | null>(null)

interface SheetProps {
  children?: React.ReactNode
  open?: boolean
  defaultOpen?: boolean
  onOpenChange?: (open: boolean) => void
}

function Sheet({ children, open, defaultOpen = false, onOpenChange }: SheetProps) {
  const [internalOpen, setInternalOpen] = React.useState(defaultOpen)
  const isOpen = open ?? internalOpen
  const setOpen = React.useCallback(
    (next: boolean) => {
      if (open === undefined) setInternalOpen(next)
      onOpenChange?.(next)
    },
    [onOpenChange, open],
  )
  return <SheetContext.Provider value={{ open: isOpen, setOpen }}>{children}</SheetContext.Provider>
}

function useSheetContext(): SheetContextValue {
  const context = React.useContext(SheetContext)
  if (context === null) throw new Error('Sheet components must be rendered inside Sheet')
  return context
}

interface AsChildProps {
  children: React.ReactNode
  asChild?: boolean
  className?: string
}

function SheetTrigger({ children }: AsChildProps) {
  const { setOpen } = useSheetContext()
  if (!React.isValidElement(children)) return children
  const child = children as React.ReactElement<{ onClick?: React.MouseEventHandler }>
  return React.cloneElement(child, {
    onClick: (event) => {
      child.props.onClick?.(event)
      if (!event.defaultPrevented) setOpen(true)
    },
  })
}

function SheetClose({ children }: AsChildProps) {
  const { setOpen } = useSheetContext()
  if (!React.isValidElement(children)) return children
  const child = children as React.ReactElement<{ onClick?: React.MouseEventHandler }>
  return React.cloneElement(child, {
    onClick: (event) => {
      child.props.onClick?.(event)
      if (!event.defaultPrevented) setOpen(false)
    },
  })
}

function SheetContent({
  className,
  children,
  side = 'right',
  showCloseButton = true,
  ...props
}: React.ComponentPropsWithoutRef<'div'> & {
  side?: 'top' | 'right' | 'bottom' | 'left'
  showCloseButton?: boolean
}) {
  const { open, setOpen } = useSheetContext()
  const horizontal = side === 'left' || side === 'right'
  const position =
    side === 'left'
      ? { left: 0, top: 0, bottom: 0 }
      : side === 'right'
        ? { right: 0, top: 0, bottom: 0 }
        : side === 'top'
          ? { left: 0, right: 0, top: 0 }
          : { left: 0, right: 0, bottom: 0 }

  return (
    <AstryxDialog
      isOpen={open}
      onOpenChange={setOpen}
      purpose="info"
      width={horizontal ? 'min(100vw, 28rem)' : '100vw'}
      maxHeight="100dvh"
      position={position}
      padding={0}
      aria-label={props['aria-label'] ?? 'Panel'}
      style={horizontal ? { height: '100dvh' } : undefined}
    >
      <div
        data-slot="sheet-content"
        className={cn('relative flex min-h-0 flex-1 flex-col gap-4 overflow-hidden', className)}
        {...props}
      >
        {children}
        {showCloseButton ? (
          <Button
            label="Close"
            variant="ghost"
            size="sm"
            icon={<XIcon />}
            isIconOnly
            onClick={() => setOpen(false)}
            className="absolute top-3 right-3"
          />
        ) : null}
      </div>
    </AstryxDialog>
  )
}

function SheetHeader({ className, ...props }: React.ComponentPropsWithoutRef<'div'>) {
  return (
    <div
      data-slot="sheet-header"
      className={cn('flex flex-col gap-1.5 p-4', className)}
      {...props}
    />
  )
}

function SheetFooter({ className, ...props }: React.ComponentPropsWithoutRef<'div'>) {
  return (
    <div
      data-slot="sheet-footer"
      className={cn('mt-auto flex flex-col gap-2 p-4', className)}
      {...props}
    />
  )
}

function SheetTitle({
  className,
  children,
  id,
}: Pick<React.ComponentPropsWithoutRef<'h2'>, 'className' | 'children' | 'id'>) {
  return (
    <Heading level={2} className={className} id={id}>
      {children}
    </Heading>
  )
}

function SheetDescription({
  className,
  children,
  id,
}: Pick<React.ComponentPropsWithoutRef<'p'>, 'className' | 'children' | 'id'>) {
  return (
    <Text type="supporting" color="secondary" display="block" className={className} id={id}>
      {children}
    </Text>
  )
}

export {
  Sheet,
  SheetTrigger,
  SheetClose,
  SheetContent,
  SheetHeader,
  SheetFooter,
  SheetTitle,
  SheetDescription,
}
