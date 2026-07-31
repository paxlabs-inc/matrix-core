/**
 * Market formatting.
 *
 * One rule governs this file: a value the vendor did not send is rendered as a
 * dash, never as a zero. `$0.00` and "we don't have it" are different facts and
 * a market surface must not conflate them — so every formatter takes
 * `number | undefined | null` and returns the placeholder for the absent case.
 */

export const ABSENT = '—'

/** The locale to format in. Falls back to the browser's, then to en-US. */
function resolveLocale(locale?: string): string | undefined {
  if (locale) return locale
  if (typeof navigator !== 'undefined' && navigator.language) return navigator.language
  return undefined
}

function present(v: number | undefined | null): v is number {
  return typeof v === 'number' && Number.isFinite(v)
}

/** A price. Sub-dollar instruments keep more precision than large caps. */
export function formatPrice(
  v: number | undefined | null,
  currency?: string,
  locale?: string,
): string {
  if (!present(v)) return ABSENT
  const abs = Math.abs(v)
  const digits = abs >= 1000 ? 2 : abs >= 1 ? 2 : abs >= 0.01 ? 4 : 8
  try {
    return new Intl.NumberFormat(resolveLocale(locale), {
      style: currency ? 'currency' : 'decimal',
      currency: currency || undefined,
      minimumFractionDigits: digits,
      maximumFractionDigits: digits,
    }).format(v)
  } catch {
    return v.toFixed(digits)
  }
}

/** A signed change, always carrying its sign so direction reads at a glance. */
export function formatChange(v: number | undefined | null, locale?: string): string {
  if (!present(v)) return ABSENT
  const body = formatPrice(Math.abs(v), undefined, locale)
  return `${v >= 0 ? '+' : '−'}${body}`
}

/** A percentage, signed. */
export function formatPercent(v: number | undefined | null, locale?: string, digits = 2): string {
  if (!present(v)) return ABSENT
  try {
    const body = new Intl.NumberFormat(resolveLocale(locale), {
      minimumFractionDigits: digits,
      maximumFractionDigits: digits,
    }).format(Math.abs(v))
    return `${v >= 0 ? '+' : '−'}${body}%`
  } catch {
    return `${v >= 0 ? '+' : '−'}${Math.abs(v).toFixed(digits)}%`
  }
}

/** A large figure — market cap, volume, revenue — in compact notation. */
export function formatCompact(v: number | undefined | null, locale?: string): string {
  if (!present(v)) return ABSENT
  try {
    return new Intl.NumberFormat(resolveLocale(locale), {
      notation: 'compact',
      maximumFractionDigits: 2,
    }).format(v)
  } catch {
    return String(Math.round(v))
  }
}

/** A whole count — shares, employees, holders. */
export function formatCount(v: number | undefined | null, locale?: string): string {
  if (!present(v)) return ABSENT
  try {
    return new Intl.NumberFormat(resolveLocale(locale), { maximumFractionDigits: 0 }).format(v)
  } catch {
    return String(Math.round(v))
  }
}

/** A ratio like a P/E. */
export function formatRatio(v: number | undefined | null, locale?: string): string {
  if (!present(v)) return ABSENT
  try {
    return new Intl.NumberFormat(resolveLocale(locale), {
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(v)
  } catch {
    return v.toFixed(2)
  }
}

/**
 * A fraction the vendor sends as a decimal (0.0036) rendered as a percentage
 * (0.36%). Kept separate from formatPercent, which takes an already-percent
 * number — mixing the two is how a yield becomes a hundred times wrong.
 */
export function formatFractionAsPercent(
  v: number | undefined | null,
  locale?: string,
  digits = 2,
): string {
  if (!present(v)) return ABSENT
  return formatPercent(v * 100, locale, digits).replace('+', '')
}

/** A low–high band, e.g. a day or 52-week range. */
export function formatRange(
  low: number | undefined | null,
  high: number | undefined | null,
  locale?: string,
): string {
  if (!present(low) || !present(high)) return ABSENT
  return `${formatPrice(low, undefined, locale)} – ${formatPrice(high, undefined, locale)}`
}

/** A timestamp as a short local time, for an as-of line. */
export function formatTime(iso: string | undefined | null, locale?: string): string {
  if (!iso) return ABSENT
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ABSENT
  try {
    return new Intl.DateTimeFormat(resolveLocale(locale), {
      hour: 'numeric',
      minute: '2-digit',
    }).format(d)
  } catch {
    return d.toISOString().slice(11, 16)
  }
}

/** A timestamp as a short date. */
export function formatDate(iso: string | undefined | null, locale?: string): string {
  if (!iso) return ABSENT
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ABSENT
  try {
    return new Intl.DateTimeFormat(resolveLocale(locale), {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    }).format(d)
  } catch {
    return d.toISOString().slice(0, 10)
  }
}

/** A timestamp as date + time, for a chart crosshair. */
export function formatDateTime(iso: string | undefined | null, locale?: string): string {
  if (!iso) return ABSENT
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ABSENT
  try {
    return new Intl.DateTimeFormat(resolveLocale(locale), {
      month: 'short',
      day: 'numeric',
      hour: 'numeric',
      minute: '2-digit',
    }).format(d)
  } catch {
    return d.toISOString().slice(0, 16).replace('T', ' ')
  }
}

/** How long ago, in plain words. */
export function formatAgo(iso: string | undefined | null, locale?: string): string {
  if (!iso) return ABSENT
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ABSENT
  const seconds = Math.max(0, Math.floor((Date.now() - d.getTime()) / 1000))
  try {
    const relative = new Intl.RelativeTimeFormat(resolveLocale(locale), {
      numeric: 'auto',
      style: 'short',
    })
    if (seconds < 60) return relative.format(0, 'second')
    const minutes = Math.floor(seconds / 60)
    if (minutes < 60) return relative.format(-minutes, 'minute')
    const hours = Math.floor(minutes / 60)
    if (hours < 24) return relative.format(-hours, 'hour')
    const days = Math.floor(hours / 24)
    if (days < 7) return relative.format(-days, 'day')
  } catch {
    if (seconds < 60) return 'now'
    const minutes = Math.floor(seconds / 60)
    if (minutes < 60) return `${minutes}m`
    const hours = Math.floor(minutes / 60)
    if (hours < 24) return `${hours}h`
    const days = Math.floor(hours / 24)
    if (days < 7) return `${days}d`
  }
  return formatDate(iso, locale)
}

/** Direction of a change, for colour and for the accessible label alike. */
export type Direction = 'up' | 'down' | 'flat'

export function directionOf(v: number | undefined | null): Direction {
  if (!present(v) || v === 0) return 'flat'
  return v > 0 ? 'up' : 'down'
}
