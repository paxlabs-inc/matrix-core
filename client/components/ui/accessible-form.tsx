'use client'

import React, { useId, forwardRef } from 'react'
import { useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'

// --- Base types ---

interface BaseFieldProps {
  label?: string
  labelKey?: string
  ns?: string
  error?: string
  errorKey?: string
  errorNs?: string
  errorParams?: Record<string, string | number | Date>
  description?: string
  descriptionKey?: string
  descriptionNs?: string
  required?: boolean
  disabled?: boolean
  className?: string
  labelClassName?: string
  errorClassName?: string
  descriptionClassName?: string
}

// --- Helper hooks ---

function useTranslatedLabel(
  label?: string,
  labelKey?: string,
  ns?: string,
  t?: (key: string) => string,
): string | undefined {
  if (label) return label
  if (labelKey && t) return t(labelKey)
  return undefined
}

function useTranslatedError(
  error?: string,
  errorKey?: string,
  ns?: string,
  errorParams?: Record<string, string | number | Date>,
  t?: (key: string, params?: Record<string, string | number | Date>) => string,
): string | undefined {
  if (error) return error
  if (errorKey && t) return t(errorKey, errorParams)
  return undefined
}

// --- FormField ---

export interface FormFieldProps extends BaseFieldProps {
  children: React.ReactNode | ((props: { id: string; errorId: string }) => React.ReactNode)
}

export function FormField({
  children,
  label,
  labelKey,
  ns,
  error,
  errorKey,
  errorNs,
  errorParams,
  description,
  descriptionKey,
  descriptionNs,
  required,
  disabled: _disabled,
  className,
  labelClassName,
  errorClassName,
  descriptionClassName,
}: FormFieldProps) {
  const t = useTranslations(ns)
  const tError = useTranslations(errorNs ?? ns)
  const tDesc = useTranslations(descriptionNs ?? ns)

  const autoId = useId()
  const fieldId = autoId
  const errorId = `${fieldId}-error`
  const descriptionId = `${fieldId}-description`

  const displayLabel = useTranslatedLabel(label, labelKey, ns, t)
  const displayError = useTranslatedError(error, errorKey, errorNs, errorParams, tError)
  const displayDescription = description ?? (descriptionKey ? tDesc(descriptionKey) : undefined)

  return (
    <div className={cn('space-y-2', className)}>
      {displayLabel && (
        <label
          htmlFor={fieldId}
          className={cn(
            'text-sm leading-none font-medium peer-disabled:cursor-not-allowed peer-disabled:opacity-70',
            labelClassName,
          )}
        >
          {displayLabel}
          {required && <span className="text-destructive ml-1">*</span>}
        </label>
      )}

      {typeof children === 'function'
        ? children({ id: fieldId, errorId: displayError ? errorId : '' })
        : children}

      {displayDescription && !displayError && (
        <p
          id={descriptionId}
          className={cn('text-muted-foreground text-[0.8rem]', descriptionClassName)}
        >
          {displayDescription}
        </p>
      )}

      {displayError && (
        <p
          id={errorId}
          role="alert"
          aria-live="polite"
          className={cn('text-destructive text-[0.8rem] font-medium', errorClassName)}
        >
          {displayError}
        </p>
      )}
    </div>
  )
}

FormField.displayName = 'FormField'

// --- TextAreaField ---

export interface TextAreaFieldProps
  extends
    BaseFieldProps,
    Omit<
      React.TextareaHTMLAttributes<HTMLTextAreaElement>,
      'id' | 'aria-describedby' | 'aria-invalid'
    > {}

export const TextAreaField = forwardRef<HTMLTextAreaElement, TextAreaFieldProps>(
  (
    {
      label,
      labelKey,
      ns,
      error,
      errorKey,
      errorNs,
      errorParams,
      description,
      descriptionKey,
      descriptionNs,
      required,
      disabled,
      className,
      labelClassName,
      errorClassName,
      descriptionClassName,
      ...textareaProps
    },
    ref,
  ) => {
    const t = useTranslations(ns)
    const tError = useTranslations(errorNs ?? ns)
    const tDesc = useTranslations(descriptionNs ?? ns)

    const autoId = useId()
    const fieldId = autoId
    const errorId = `${fieldId}-error`
    const descriptionId = `${fieldId}-description`

    const displayLabel = useTranslatedLabel(label, labelKey, ns, t)
    const displayError = useTranslatedError(error, errorKey, errorNs, errorParams, tError)
    const displayDescription = description ?? (descriptionKey ? tDesc(descriptionKey) : undefined)

    const ariaDescribedBy =
      cn(displayError ? errorId : undefined, displayDescription ? descriptionId : undefined) ||
      undefined

    return (
      <div className={cn('space-y-2', className)}>
        {displayLabel && (
          <label
            htmlFor={fieldId}
            className={cn(
              'text-sm leading-none font-medium peer-disabled:cursor-not-allowed peer-disabled:opacity-70',
              labelClassName,
            )}
          >
            {displayLabel}
            {required && <span className="text-destructive ml-1">*</span>}
          </label>
        )}

        <textarea
          ref={ref}
          id={fieldId}
          aria-describedby={ariaDescribedBy}
          aria-invalid={!!displayError}
          aria-required={required}
          disabled={disabled}
          className={cn(
            'border-input bg-background ring-offset-background placeholder:text-muted-foreground focus-visible:ring-ring flex min-h-[80px] w-full rounded-md border px-3 py-2 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50',
            displayError && 'border-destructive focus-visible:ring-destructive',
          )}
          {...textareaProps}
        />

        {displayDescription && !displayError && (
          <p
            id={descriptionId}
            className={cn('text-muted-foreground text-[0.8rem]', descriptionClassName)}
          >
            {displayDescription}
          </p>
        )}

        {displayError && (
          <p
            id={errorId}
            role="alert"
            aria-live="polite"
            className={cn('text-destructive text-[0.8rem] font-medium', errorClassName)}
          >
            {displayError}
          </p>
        )}
      </div>
    )
  },
)

TextAreaField.displayName = 'TextAreaField'

// --- SelectField ---

export interface SelectOption {
  value: string
  label?: string
  labelKey?: string
  disabled?: boolean
}

export interface SelectFieldProps
  extends
    BaseFieldProps,
    Omit<
      React.SelectHTMLAttributes<HTMLSelectElement>,
      'id' | 'aria-describedby' | 'aria-invalid' | 'children'
    > {
  options: SelectOption[]
  onValueChange?: (value: string) => void
  placeholder?: string
  placeholderKey?: string
}

export const SelectField = forwardRef<HTMLSelectElement, SelectFieldProps>(
  (
    {
      options,
      label,
      labelKey,
      ns,
      error,
      errorKey,
      errorNs,
      errorParams,
      description,
      descriptionKey,
      descriptionNs,
      required,
      disabled,
      className,
      labelClassName,
      errorClassName,
      descriptionClassName,
      onValueChange,
      placeholder,
      placeholderKey,
      onChange,
      ...selectProps
    },
    ref,
  ) => {
    const t = useTranslations(ns)
    const tError = useTranslations(errorNs ?? ns)
    const tDesc = useTranslations(descriptionNs ?? ns)

    const autoId = useId()
    const fieldId = autoId
    const errorId = `${fieldId}-error`
    const descriptionId = `${fieldId}-description`

    const displayLabel = useTranslatedLabel(label, labelKey, ns, t)
    const displayError = useTranslatedError(error, errorKey, errorNs, errorParams, tError)
    const displayDescription = description ?? (descriptionKey ? tDesc(descriptionKey) : undefined)
    const displayPlaceholder = placeholder ?? (placeholderKey ? t(placeholderKey) : undefined)

    const ariaDescribedBy =
      cn(displayError ? errorId : undefined, displayDescription ? descriptionId : undefined) ||
      undefined

    return (
      <div className={cn('space-y-2', className)}>
        {displayLabel && (
          <label
            htmlFor={fieldId}
            className={cn(
              'text-sm leading-none font-medium peer-disabled:cursor-not-allowed peer-disabled:opacity-70',
              labelClassName,
            )}
          >
            {displayLabel}
            {required && <span className="text-destructive ml-1">*</span>}
          </label>
        )}

        <select
          {...selectProps}
          ref={ref}
          id={fieldId}
          aria-describedby={ariaDescribedBy}
          aria-invalid={!!displayError}
          aria-required={required}
          disabled={disabled}
          onChange={(e) => {
            onChange?.(e)
            onValueChange?.(e.target.value)
          }}
          className={cn(
            'border-input bg-background ring-offset-background placeholder:text-muted-foreground focus:ring-ring flex h-10 w-full items-center justify-between rounded-md border px-3 py-2 text-sm focus:ring-2 focus:ring-offset-2 focus:outline-none disabled:cursor-not-allowed disabled:opacity-50 [&>span]:line-clamp-1',
            displayError && 'border-destructive focus-visible:ring-destructive',
          )}
        >
          {displayPlaceholder && (
            <option value="" disabled>
              {displayPlaceholder}
            </option>
          )}
          {options.map((opt) => {
            const optLabel = opt.label ?? (opt.labelKey ? t(opt.labelKey) : opt.value)
            return (
              <option key={opt.value} value={opt.value} disabled={opt.disabled}>
                {optLabel}
              </option>
            )
          })}
        </select>

        {displayDescription && !displayError && (
          <p
            id={descriptionId}
            className={cn('text-muted-foreground text-[0.8rem]', descriptionClassName)}
          >
            {displayDescription}
          </p>
        )}

        {displayError && (
          <p
            id={errorId}
            role="alert"
            aria-live="polite"
            className={cn('text-destructive text-[0.8rem] font-medium', errorClassName)}
          >
            {displayError}
          </p>
        )}
      </div>
    )
  },
)

SelectField.displayName = 'SelectField'
