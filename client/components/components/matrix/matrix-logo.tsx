import Image from 'next/image'
import { cn } from '@/lib/utils'

type LogoSize = 'sm' | 'md' | 'lg' | 'xl'

/** Per-size icon px + wordmark text class. The mark is the brand SVG in
 *  /public; the wordmark is live text so it always tracks the theme. */
const SIZES: Record<LogoSize, { icon: number; word: string }> = {
  sm: { icon: 24, word: 'text-base' },
  md: { icon: 32, word: 'text-xl' },
  lg: { icon: 44, word: 'text-2xl' },
  xl: { icon: 64, word: 'text-4xl' },
}

/**
 * MatrixLogo — the brand lockup: the Matrix icon mark + the "matrix"
 * wordmark. Sized via the `size` prop (defaults to `md`) so headers, hero
 * screens and the sidebar can each render it at a legible scale instead of
 * the old hardcoded 20px glyph. Pass `iconOnly` for tight/collapsed spots.
 */
export function MatrixLogo({
  className,
  size = 'md',
  iconOnly = false,
}: {
  className?: string
  size?: LogoSize
  iconOnly?: boolean
}) {
  const { icon, word } = SIZES[size]
  return (
    <span className={cn('inline-flex items-center gap-2.5', className)}>
      <Image
        src="/matrix_icon_2.svg"
        alt="Matrix"
        width={icon}
        height={icon}
        priority
        className="shrink-0"
      />
      {!iconOnly && (
        <span className={cn('text-foreground font-semibold tracking-tight', word)}>matrix</span>
      )}
    </span>
  )
}
