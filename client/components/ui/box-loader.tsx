import type React from 'react'
import { cn } from '@/lib/utils'

/**
 * 3D rolling-boxes loader. The animation translates the cubes well
 * outside the base footprint, so it is wrapped in a fixed-size, centered,
 * overflow-visible "stage" (.box-loader) that reserves enough room — this
 * is what stops it being clipped inside small containers like the chat
 * bubble. Pass `size="lg"` for the full-screen provisioning view.
 */
const Loader: React.FC<{ className?: string; size?: 'sm' | 'lg' }> = ({
  className,
  size = 'sm',
}) => {
  return (
    <div className={cn('box-loader', size === 'lg' && 'box-loader--lg', className)}>
      <div className="boxes">
        <div className="box box-1">
          <div className="face face-front" />
          <div className="face face-right" />
          <div className="face face-top" />
          <div className="face face-back" />
        </div>
        <div className="box box-2">
          <div className="face face-front" />
          <div className="face face-right" />
          <div className="face face-top" />
          <div className="face face-back" />
        </div>
        <div className="box box-3">
          <div className="face face-front" />
          <div className="face face-right" />
          <div className="face face-top" />
          <div className="face face-back" />
        </div>
        <div className="box box-4">
          <div className="face face-front" />
          <div className="face face-right" />
          <div className="face face-top" />
          <div className="face face-back" />
        </div>
      </div>
    </div>
  )
}

export default Loader
