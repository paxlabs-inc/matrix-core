'use client'

import * as React from 'react'
import { Dialog as AstryxDialog } from '@astryxdesign/core/Dialog'
import { Heading, Text } from '@astryxdesign/core/Text'
import { Button } from '@astryxdesign/core/Button'
import { XIcon } from '@/lib/matrix-icons'
import { cn } from '@/lib/utils'

interface DialogContextValue {
  open: boolean
  setOpen(open: boolean): void
}

const DialogContext = React.createContext<DialogContextValue | null>(null)

interface DialogProps {
  children?: React.ReactNode
  open?: boolean
  defaultOpen?: boolean
  onOpenChange?: (open: boolean) => void
}

function Dialog({ children, open, defaultOpen = false, onOpenChange }: DialogProps) {
  const [internalOpen, setInternalOpen] = React.useState(defaultOpen)
  const isOpen = open ?? internalOpen
  const setOpen = React.useCallback(
    (next: boolean) => {
      if (open === undefined) setInternalOpen(next)
      onOpenChange?.(next)
    },
    [onOpenChange, open],
  )
  return (
    <DialogContext.Provider value={{ open: isOpen, setOpen }}>{children}</DialogContext.Provider>
  )
}

function useDialogContext(): DialogContextValue {
  const context = React.useContext(DialogContext)
  if (context === null) throw new Error('Dialog components must be rendered inside Dialog')
  return context
}

interface AsChildProps {
  children: React.ReactNode
  asChild?: boolean
  className?: string
}

function DialogTrigger({ children }: AsChildProps) {
  const { setOpen } = useDialogContext()
  if (!React.isValidElement(children)) return children
  const child = children as React.ReactElement<{ onClick?: React.MouseEventHandler }>
  return React.cloneElement(child, {
    onClick: (event) => {
      child.props.onClick?.(event)
      if (!event.defaultPrevented) setOpen(true)
    },
  })
}

function DialogClose({ children }: AsChildProps) {
  const { setOpen } = useDialogContext()
  if (!React.isValidElement(children)) return children
  const child = children as React.ReactElement<{ onClick?: React.MouseEventHandler }>
  return React.cloneElement(child, {
    onClick: (event) => {
      child.props.onClick?.(event)
      if (!event.defaultPrevented) setOpen(false)
    },
  })
}

function DialogPortal({ children }: { children?: React.ReactNode }) {
  return children
}

function DialogOverlay() {
  return null
}

function DialogContent({
  className,
  children,
  showCloseButton = true,
  ...props
}: React.ComponentPropsWithoutRef<'div'> & {
  showCloseButton?: boolean
}) {
  const { open, setOpen } = useDialogContext()
  return (
    <AstryxDialog
      isOpen={open}
      onOpenChange={setOpen}
      purpose="info"
      width="min(calc(100vw - 2rem), 36rem)"
      maxHeight="min(92dvh, 54rem)"
      padding={0}
      aria-label={props['aria-label'] ?? 'Dialog'}
    >
      <div
        data-slot="dialog-content"
        className={cn('relative flex min-h-0 flex-col gap-4 overflow-y-auto p-6', className)}
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

function DialogHeader({ className, ...props }: React.ComponentPropsWithoutRef<'div'>) {
  return (
    <div
      data-slot="dialog-header"
      className={cn('flex flex-col gap-2 text-left', className)}
      {...props}
    />
  )
}

function DialogFooter({ className, ...props }: React.ComponentPropsWithoutRef<'div'>) {
  return (
    <div
      data-slot="dialog-footer"
      className={cn('flex flex-col-reverse gap-2 sm:flex-row sm:justify-end', className)}
      {...props}
    />
  )
}

function DialogTitle({
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

function DialogDescription({
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
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogOverlay,
  DialogPortal,
  DialogTitle,
  DialogTrigger,
}
