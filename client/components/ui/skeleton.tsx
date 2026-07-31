import { Skeleton as AstryxSkeleton } from '@astryxdesign/core/Skeleton'

function Skeleton({ className, ...props }: React.ComponentProps<'div'>) {
  return <AstryxSkeleton data-slot="skeleton" radius={2} className={className} {...props} />
}

export { Skeleton }
