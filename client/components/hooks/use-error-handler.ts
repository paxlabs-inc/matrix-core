import { useCallback, useState } from 'react'
import { useTranslations } from 'next-intl'

type TFunction = ReturnType<typeof useTranslations>

export interface ErrorHandlerOptions {
  /** i18n namespace for error messages */
  ns?: string
  /** Custom error-to-message mapper */
  mapError?: (error: unknown) => string | undefined
  /** Called after an error is set */
  onError?: (error: unknown) => void
}

export interface ErrorHandlerState {
  /** Current error object */
  error: unknown
  /** Localized error message for display */
  errorMessage: string | undefined
  /** Whether an error is present */
  hasError: boolean
  /** Set an error manually */
  setError: (error: unknown) => void
  /** Clear the current error */
  clearError: () => void
  /** Handle an error: set state + return localized message */
  handleError: (error: unknown) => string | undefined
}

/**
 * Maps common error types to i18n translation keys.
 * Keys are looked up in the given namespace.
 *
 * Key patterns:
 * - errors.network        → network/fetch failures
 * - errors.unauthorized   → 401
 * - errors.forbidden      → 403
 * - errors.notFound       → 404
 * - errors.timeout        → timeout
 * - errors.server         → 5xx
 * - errors.unknown        → fallback
 */
function getErrorTranslationKey(error: unknown): string {
  if (error instanceof TypeError && error.message.includes('fetch')) {
    return 'errors.network'
  }

  if (error instanceof DOMException && error.name === 'AbortError') {
    return 'errors.timeout'
  }

  if (error instanceof Response) {
    switch (true) {
      case error.status === 401:
        return 'errors.unauthorized'
      case error.status === 403:
        return 'errors.forbidden'
      case error.status === 404:
        return 'errors.notFound'
      case error.status >= 500:
        return 'errors.server'
      default:
        return 'errors.unknown'
    }
  }

  if (
    typeof error === 'object' &&
    error !== null &&
    'status' in error &&
    typeof (error as Record<string, unknown>).status === 'number'
  ) {
    const status = (error as Record<string, unknown>).status as number
    switch (true) {
      case status === 401:
        return 'errors.unauthorized'
      case status === 403:
        return 'errors.forbidden'
      case status === 404:
        return 'errors.notFound'
      case status >= 500:
        return 'errors.server'
      default:
        return 'errors.unknown'
    }
  }

  return 'errors.unknown'
}

/**
 * Resolves an error to a localized message string.
 */
function resolveErrorMessage(
  error: unknown,
  t: TFunction,
  mapError?: (error: unknown) => string | undefined,
): string | undefined {
  if (!error) return undefined

  if (mapError) {
    const custom = mapError(error)
    if (custom) return custom
  }

  if (typeof error === 'string') return error

  if (error instanceof Error && error.message) {
    const key = getErrorTranslationKey(error)
    const translated = t(key)
    return translated !== key ? translated : error.message
  }

  const key = getErrorTranslationKey(error)
  const translated = t(key)
  return translated !== key ? translated : t('errors.unknown')
}

/**
 * Hook for localized error handling in forms and async operations.
 *
 * @example
 * ```tsx
 * const { errorMessage, handleError, clearError } = useErrorHandler({ ns: 'common' })
 *
 * try {
 *   await submitForm(data)
 * } catch (err) {
 *   const msg = handleError(err)
 *   toast.error(msg)
 * }
 * ```
 */
export function useErrorHandler(options: ErrorHandlerOptions = {}): ErrorHandlerState {
  const { ns, mapError, onError } = options
  const t = useTranslations(ns)
  const [error, setErrorState] = useState<unknown>(null)

  const setError = useCallback(
    (err: unknown) => {
      setErrorState(err)
      onError?.(err)
    },
    [onError],
  )

  const clearError = useCallback(() => {
    setErrorState(null)
  }, [])

  const handleError = useCallback(
    (err: unknown) => {
      setErrorState(err)
      onError?.(err)
      return resolveErrorMessage(err, t, mapError)
    },
    [t, mapError, onError],
  )

  const errorMessage = resolveErrorMessage(error, t, mapError)

  return {
    error,
    errorMessage,
    hasError: error !== null && error !== undefined,
    setError,
    clearError,
    handleError,
  }
}
