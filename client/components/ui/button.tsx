import * as React from 'react'
import { cva, type VariantProps } from 'class-variance-authority'
import { Button as AstryxButton } from '@astryxdesign/core/Button'

import { cn } from '@/lib/utils'

const buttonVariants = cva(
  "inline-flex shrink-0 items-center justify-center gap-2 rounded-md text-sm font-medium whitespace-nowrap transition-all outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:pointer-events-none disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default: 'bg-primary text-primary-foreground hover:bg-primary/90',
        destructive:
          'bg-destructive text-white hover:bg-destructive/90 focus-visible:ring-destructive/20 dark:bg-destructive/60 dark:focus-visible:ring-destructive/40',
        outline:
          'border bg-background shadow-xs hover:bg-accent hover:text-accent-foreground dark:border-input dark:bg-input/30 dark:hover:bg-input/50',
        secondary: 'bg-secondary text-secondary-foreground hover:bg-secondary/80',
        ghost: 'hover:bg-accent hover:text-accent-foreground dark:hover:bg-accent/50',
        link: 'text-primary underline-offset-4 hover:underline',
      },
      size: {
        default: 'h-9 px-4 py-2 has-[>svg]:px-3',
        xs: "h-6 gap-1 rounded-md px-2 text-xs has-[>svg]:px-1.5 [&_svg:not([class*='size-'])]:size-3",
        sm: 'h-8 gap-1.5 rounded-md px-3 has-[>svg]:px-2.5',
        lg: 'h-10 rounded-md px-6 has-[>svg]:px-4',
        icon: 'size-9',
        'icon-xs': "size-6 rounded-md [&_svg:not([class*='size-'])]:size-3",
        'icon-sm': 'size-8',
        'icon-lg': 'size-10',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
)

type LegacyButtonProps = React.ComponentProps<'button'> &
  VariantProps<typeof buttonVariants> & {
    asChild?: boolean
  }

function textFromNode(node: React.ReactNode): string {
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(textFromNode).join(' ').trim()
  if (React.isValidElement(node)) {
    return textFromNode((node.props as { children?: React.ReactNode }).children)
  }
  return ''
}

function isLikelyIcon(node: React.ReactNode): node is React.ReactElement {
  if (!React.isValidElement(node)) return false

  const props = node.props as {
    children?: React.ReactNode
    className?: string
    'data-icon'?: unknown
    'aria-hidden'?: unknown
  }

  return (
    node.type === 'svg' ||
    props['data-icon'] !== undefined ||
    props['aria-hidden'] !== undefined ||
    /\b(?:size|h|w)-/.test(props.className ?? '') ||
    (typeof node.type !== 'string' && props.children == null)
  )
}

function astryxVariant(
  variant: LegacyButtonProps['variant'],
): 'primary' | 'secondary' | 'ghost' | 'destructive' {
  if (variant === 'destructive') return 'destructive'
  if (variant === 'ghost' || variant === 'link') return 'ghost'
  if (variant === 'outline' || variant === 'secondary') return 'secondary'
  return 'primary'
}

function astryxSize(size: LegacyButtonProps['size']): 'sm' | 'md' | 'lg' {
  if (size === 'xs' || size === 'sm' || size === 'icon-xs' || size === 'icon-sm') return 'sm'
  if (size === 'lg' || size === 'icon-lg') return 'lg'
  return 'md'
}

const Button = React.forwardRef<HTMLButtonElement, LegacyButtonProps>(function Button(
  {
    className,
    variant = 'default',
    size = 'default',
    asChild = false,
    children,
    disabled,
    'aria-label': ariaLabel,
    ...props
  },
  ref,
) {
  const iconOnly = typeof size === 'string' && size.startsWith('icon')
  const child = asChild && React.isValidElement(children) ? children : null
  const childProps = child?.props as
    | {
        children?: React.ReactNode
        href?: string
        target?: string
        rel?: string
        onClick?: React.MouseEventHandler<HTMLButtonElement>
      }
    | undefined
  const content = childProps?.children ?? children
  const contentNodes = React.Children.toArray(content)
  const leadingIcon =
    !iconOnly && contentNodes.length > 1 && isLikelyIcon(contentNodes[0])
      ? contentNodes[0]
      : undefined
  const visibleContent = leadingIcon
    ? contentNodes.length === 2
      ? contentNodes[1]
      : contentNodes.slice(1)
    : content
  const label = ariaLabel || textFromNode(content) || 'Action'
  const fillsContainer = /\bw-full\b/.test(className ?? '')

  return (
    <AstryxButton
      ref={ref}
      data-slot="button"
      data-variant={variant}
      data-size={size}
      label={label}
      variant={astryxVariant(variant)}
      size={astryxSize(size)}
      isDisabled={disabled}
      isIconOnly={iconOnly}
      icon={iconOnly ? content : leadingIcon}
      width={fillsContainer ? '100%' : undefined}
      href={childProps?.href}
      target={childProps?.target}
      rel={childProps?.rel}
      onClick={childProps?.onClick ?? props.onClick}
      className={cn(className)}
      {...props}
    >
      {iconOnly ? undefined : visibleContent}
    </AstryxButton>
  )
})

export { Button, buttonVariants }
