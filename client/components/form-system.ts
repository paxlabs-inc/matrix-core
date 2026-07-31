/**
 * Form System - barrel export for all form-related components and utilities.
 *
 * Usage:
 * ```tsx
 * import { FormField, TextAreaField, SelectField, useErrorHandler, validation, zodResolver } from '@/components/form-system'
 * ```
 */

// Form components
export {
  FormField,
  TextAreaField,
  SelectField,
  type FormFieldProps,
  type TextAreaFieldProps,
  type SelectFieldProps,
  type SelectOption,
} from '@/components/ui/accessible-form'

// Error handling
export {
  useErrorHandler,
  type ErrorHandlerOptions,
  type ErrorHandlerState,
} from '@/components/hooks/use-error-handler'

// Validation utilities
export { zodResolver, validation, confirmMatch } from '@/components/validation'
