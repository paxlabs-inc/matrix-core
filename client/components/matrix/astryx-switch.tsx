'use client'

import { Switch as AstryxSwitch } from '@astryxdesign/core/Switch'
import { useState } from 'react'

type MatrixSwitchProps = {
  checked?: boolean
  defaultChecked?: boolean
  onCheckedChange?: (checked: boolean) => void
  disabled?: boolean
  className?: string
  id?: string
  name?: string
  'aria-label'?: string
}

/**
 * Compatibility seam for existing Centra AI controlled-switch call sites.
 * Astryx owns rendering, focus, keyboard, loading, and theme behavior.
 */
export function Switch({
  checked,
  defaultChecked = false,
  onCheckedChange,
  disabled,
  className,
  id,
  name,
  'aria-label': ariaLabel,
}: MatrixSwitchProps) {
  const [internalValue, setInternalValue] = useState(defaultChecked)
  const value = checked ?? internalValue

  return (
    <AstryxSwitch
      label={ariaLabel || 'Toggle'}
      isLabelHidden
      value={value}
      onChange={(next) => {
        if (checked === undefined) setInternalValue(next)
        onCheckedChange?.(next)
      }}
      isDisabled={disabled}
      className={className}
      id={id}
      htmlName={name}
    />
  )
}
