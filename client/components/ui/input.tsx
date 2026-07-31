import * as React from 'react'
import { TextInput } from '@astryxdesign/core/TextInput'

import { cn } from '@/lib/utils'

const Input = React.forwardRef<HTMLInputElement, React.ComponentProps<'input'>>(function Input(
  {
    className,
    type = 'text',
    value,
    defaultValue,
    onChange,
    disabled,
    required,
    autoFocus,
    name,
    placeholder,
    'aria-label': ariaLabel,
    size: _htmlSize,
    style,
    ...props
  },
  ref,
) {
  const [internalValue, setInternalValue] = React.useState(() =>
    defaultValue == null ? '' : String(defaultValue),
  )
  const controlled = value !== undefined
  const stringValue = controlled ? String(value ?? '') : internalValue

  if (type === 'text' || type === 'search' || type === 'password' || type === 'email') {
    return (
      <TextInput
        ref={ref}
        type={(type === 'search' ? 'text' : type) as 'text' | 'password' | 'email'}
        label={ariaLabel || placeholder || name || 'Input'}
        isLabelHidden
        value={stringValue}
        onChange={(next, event) => {
          if (!controlled) setInternalValue(next)
          onChange?.(event)
        }}
        isDisabled={disabled}
        isRequired={required}
        hasAutoFocus={autoFocus}
        htmlName={name}
        placeholder={placeholder}
        width="100%"
        className={cn(
          'bg-muted/60 focus-within:bg-muted hover:bg-muted/80 transition-colors',
          className,
        )}
        style={{
          ...style,
          borderWidth: 0,
          borderStyle: 'none',
          boxShadow: 'none',
        }}
        {...props}
      />
    )
  }

  return (
    <input
      type={type}
      ref={ref}
      value={value}
      defaultValue={defaultValue}
      onChange={onChange}
      disabled={disabled}
      required={required}
      autoFocus={autoFocus}
      name={name}
      placeholder={placeholder}
      aria-label={ariaLabel}
      data-slot="input"
      className={cn(
        'file:text-foreground placeholder:text-muted-foreground selection:bg-primary selection:text-primary-foreground bg-muted/60 focus-visible:bg-muted h-9 w-full min-w-0 rounded-md px-3 py-1 text-base transition-colors outline-none file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 md:text-sm',
        'aria-invalid:bg-destructive/10',
        className,
      )}
      style={style}
      {...props}
    />
  )
})

export { Input }
