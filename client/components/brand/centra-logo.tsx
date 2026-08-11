import Image from 'next/image'
import { BRAND_NAME } from '@/lib/brand'
import { cn } from '@/lib/utils'

export type LogoSize = 'sm' | 'md' | 'lg' | 'xl'

const SIZES: Record<LogoSize, { mark: string; word: string }> = {
  sm: { mark: 'size-5', word: 'text-base' },
  md: { mark: 'size-6', word: 'text-xl' },
  lg: { mark: 'size-8', word: 'text-2xl' },
  xl: { mark: 'size-12', word: 'text-4xl' },
}

export function CentraLogo({
  className,
  size = 'md',
  iconOnly = false,
}: {
  className?: string
  size?: LogoSize
  iconOnly?: boolean
}) {
  const { mark, word } = SIZES[size]

  return (
    <span
      role="img"
      aria-label={BRAND_NAME}
      className={cn('inline-flex items-center gap-2.5', className)}
    >
      <Image
        aria-hidden
        alt=""
        className={cn('shrink-0', mark)}
        height={48}
        src="/centra-icon.svg"
        width={48}
      />
      {!iconOnly && (
        <span className={cn('text-foreground font-semibold tracking-tight', word)}>
          {BRAND_NAME}
        </span>
      )}
    </span>
  )
}
