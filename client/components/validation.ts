import { z } from 'zod'
import { zodResolver } from '@hookform/resolvers/zod'

// Re-export zodResolver for convenient access from form-system
export { zodResolver }

/**
 * Common Zod validation schemas for form fields.
 *
 * All schemas use `.superRefine` to return i18n-friendly error messages.
 * Consumers should provide their own `t` function or use zod-i18n-map.
 */
export const validation = {
  /** Required string (non-empty after trim) */
  requiredString: (opts?: { min?: number; max?: number }) =>
    z
      .string()
      .trim()
      .min(opts?.min ?? 1, { message: 'validation.required' })
      .max(opts?.max ?? 10000, { message: 'validation.maxLength' }),

  /** Optional string (allows empty) */
  optionalString: (opts?: { max?: number }) =>
    z
      .string()
      .trim()
      .max(opts?.max ?? 10000, { message: 'validation.maxLength' })
      .optional()
      .or(z.literal('')),

  /** Email with basic format check */
  email: () =>
    z
      .string()
      .trim()
      .min(1, { message: 'validation.required' })
      .email({ message: 'validation.email' }),

  /** URL with protocol check */
  url: () =>
    z
      .string()
      .trim()
      .min(1, { message: 'validation.required' })
      .url({ message: 'validation.url' })
      .refine((val) => val.startsWith('http://') || val.startsWith('https://'), {
        message: 'validation.url',
      }),

  /** Integer within optional range */
  integer: (opts?: { min?: number; max?: number }) =>
    z.coerce
      .number()
      .int({ message: 'validation.integer' })
      .min(opts?.min ?? -Infinity, { message: 'validation.min' })
      .max(opts?.max ?? Infinity, { message: 'validation.max' }),

  /** Number within optional range */
  number: (opts?: { min?: number; max?: number }) =>
    z.coerce
      .number()
      .min(opts?.min ?? -Infinity, { message: 'validation.min' })
      .max(opts?.max ?? Infinity, { message: 'validation.max' }),

  /** Boolean (checkbox) */
  boolean: () => z.boolean(),

  /** Required boolean (must be true, e.g. terms acceptance) */
  requiredTrue: () =>
    z.literal(true, {
      errorMap: () => ({ message: 'validation.requiredTrue' }),
    }),

  /** Select field (non-empty string) */
  select: () => z.string().min(1, { message: 'validation.required' }),

  /** Minimum length string */
  minLength: (min: number) => z.string().trim().min(min, { message: 'validation.minLength' }),

  /** Maximum length string */
  maxLength: (max: number) => z.string().trim().max(max, { message: 'validation.maxLength' }),

  /** Password with minimum strength */
  password: (opts?: { min?: number }) =>
    z
      .string()
      .min(opts?.min ?? 8, { message: 'validation.minLength' })
      .regex(/[A-Z]/, { message: 'validation.password.uppercase' })
      .regex(/[a-z]/, { message: 'validation.password.lowercase' })
      .regex(/[0-9]/, { message: 'validation.password.number' }),

  /** Confirm password (matches another field) */
  confirmPassword: (_passwordField: string = 'password') =>
    z.string().superRefine((val, ctx) => {
      // This should be used within a .refine() on the parent schema
      // for cross-field validation
      if (!val || val.length === 0) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'validation.required',
        })
      }
    }),
} as const

/**
 * Helper to create a "confirm password" refinement on a schema.
 *
 * @example
 * ```ts
 * const schema = z.object({
 *   password: z.string(),
 *   confirmPassword: z.string(),
 * }).refine(confirmMatch('password', 'confirmPassword'), {
 *   message: 'validation.password.match',
 *   path: ['confirmPassword'],
 * })
 * ```
 */
export function confirmMatch(
  passwordField: string,
  confirmField: string,
): (data: Record<string, unknown>) => boolean {
  return (data) => data[passwordField] === data[confirmField]
}
