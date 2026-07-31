import * as React from 'react'
import { TextArea } from '@astryxdesign/core/TextArea'

const Textarea = React.forwardRef<HTMLTextAreaElement, React.ComponentProps<'textarea'>>(
  function Textarea(
    {
      className,
      value,
      defaultValue,
      onChange,
      disabled,
      required,
      autoFocus,
      name,
      placeholder,
      rows,
      spellCheck,
      'aria-label': ariaLabel,
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

    return (
      <TextArea
        ref={ref}
        label={ariaLabel || placeholder || name || 'Message'}
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
        rows={rows}
        hasSpellCheck={
          spellCheck === undefined ? undefined : spellCheck === true || spellCheck === 'true'
        }
        width="100%"
        className={[
          'bg-muted/60 focus-within:bg-muted hover:bg-muted/80 transition-colors',
          className,
        ]
          .filter(Boolean)
          .join(' ')}
        style={{
          ...style,
          borderWidth: 0,
          borderStyle: 'none',
          boxShadow: 'none',
        }}
        {...props}
      />
    )
  },
)

export { Textarea }
