/**
 * Zero-dependency formatting helpers, ported verbatim from
 * Paxport-Mobile-Wallet/src/lib/format.ts so balance/USD/price rendering
 * matches the wallet app exactly.
 */

export function shortenAddress(addr: string, chars = 4): string {
  if (!addr) return ''
  return `${addr.slice(0, chars + 2)}...${addr.slice(-chars)}`
}

export function formatBalance(wei: string | bigint, decimals = 18, display = 4): string {
  if (typeof wei === 'string' && wei.includes('.')) {
    const parts = wei.split('.')
    const whole = parts[0] ?? '0'
    const frac = (parts[1] ?? '').slice(0, display).padEnd(display, '0')
    return `${whole}.${frac}`
  }

  try {
    const raw = typeof wei === 'string' ? BigInt(wei) : wei
    const divisor = BigInt('1' + '0'.repeat(decimals))
    const whole = raw / divisor
    const frac = raw % divisor
    const fracStr = frac.toString().padStart(decimals, '0').slice(0, display)
    return `${whole}.${fracStr}`
  } catch {
    return `0.${'0'.repeat(display)}`
  }
}

/** Format an amount that is already expressed in whole tokens. Keeps useful
 * precision, removes padded zeroes, and groups the integer portion without
 * converting through Number (which would lose precision on large balances). */
export function formatDecimalAmount(
  amount: string | number,
  maximumFractionDigits = 6,
  locale = 'en-US',
): string {
  const raw = String(amount).trim()
  if (!raw) return '0'

  const negative = raw.startsWith('-')
  const body = negative ? raw.slice(1) : raw
  const [wholeRaw = '0', fractionRaw = ''] = body.split('.')
  const digits = wholeRaw.replace(/[^0-9]/g, '') || '0'
  const whole = digits.replace(/^0+(?=\d)/, '')
  const fraction = fractionRaw
    .replace(/[^0-9]/g, '')
    .slice(0, Math.max(0, maximumFractionDigits))
    .replace(/0+$/, '')

  const group = new Intl.NumberFormat(locale, {
    useGrouping: true,
    maximumFractionDigits: 0,
  }).format(BigInt(whole))

  return `${negative && (whole !== '0' || fraction) ? '-' : ''}${group}${fraction ? `.${fraction}` : ''}`
}

/** Convert a base-unit integer (wei for 18-decimal PAX) into a readable token
 * amount. Decimal inputs are treated as already converted to avoid applying
 * the token divisor twice. */
export function formatTokenAmount(
  amount: string | bigint,
  decimals = 18,
  maximumFractionDigits = 6,
  locale = 'en-US',
): string {
  if (typeof amount === 'string' && amount.includes('.')) {
    return formatDecimalAmount(amount, maximumFractionDigits, locale)
  }
  try {
    const raw = typeof amount === 'string' ? BigInt(amount) : amount
    const negative = raw < BigInt(0)
    const absolute = negative ? -raw : raw
    const safeDecimals = Math.max(0, decimals)
    const divisor = BigInt(`1${'0'.repeat(safeDecimals)}`)
    const whole = absolute / divisor
    const fraction =
      safeDecimals === 0
        ? ''
        : (absolute % divisor)
            .toString()
            .padStart(safeDecimals, '0')
            .slice(0, Math.max(0, maximumFractionDigits))
    return formatDecimalAmount(
      `${negative ? '-' : ''}${whole}${fraction ? `.${fraction}` : ''}`,
      maximumFractionDigits,
      locale,
    )
  } catch {
    return '0'
  }
}

/** Inverse of formatBalance: turn a human decimal amount (e.g. "1.5") into a
 *  base-unit integer string (e.g. wei). */
export function parseUnits(amount: string, decimals = 18): string {
  const trimmed = (amount ?? '').trim()
  if (trimmed === '') return '0'
  const negative = trimmed.startsWith('-')
  const body = negative ? trimmed.slice(1) : trimmed
  const dot = body.indexOf('.')
  const whole = (dot === -1 ? body : body.slice(0, dot)).replace(/[^0-9]/g, '')
  const fracRaw = (dot === -1 ? '' : body.slice(dot + 1)).replace(/[^0-9]/g, '')
  const frac = fracRaw.slice(0, decimals).padEnd(decimals, '0')
  try {
    const value = BigInt(`${whole || '0'}${frac}`)
    return negative && value !== BigInt(0) ? `-${value.toString()}` : value.toString()
  } catch {
    return '0'
  }
}

/** Format USD from integer cents (string | bigint) without float math. */
export function formatUsdCents(cents: string | bigint, locale = 'en-US'): string {
  try {
    const raw = typeof cents === 'string' ? BigInt(cents || '0') : cents
    const negative = raw < BigInt(0)
    const abs = negative ? -raw : raw
    const whole = abs / BigInt(100)
    const frac = (abs % BigInt(100)).toString().padStart(2, '0')
    const num = Number(`${negative ? '-' : ''}${whole}.${frac}`)
    return new Intl.NumberFormat(locale, {
      style: 'currency',
      currency: 'USD',
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(num)
  } catch {
    return '$0.00'
  }
}

/** Format a USD amount already in whole dollars (display-only; prefer cents). */
export function formatUsd(value: number, locale = 'en-US'): string {
  return new Intl.NumberFormat(locale, {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value)
}

/** Display USD from a dollar amount string (e.g. receipt costs). */
export function formatUsdFromDecimal(amount: string | number, locale = 'en-US'): string {
  if (typeof amount === 'number') return formatUsd(amount, locale)
  const trimmed = amount.trim()
  if (!trimmed || trimmed === '.') return formatUsd(0, locale)
  const negative = trimmed.startsWith('-')
  const body = negative ? trimmed.slice(1) : trimmed
  const [wholeRaw = '0', fracRaw = ''] = body.split('.')
  const whole = wholeRaw.replace(/[^0-9]/g, '') || '0'
  const frac = fracRaw
    .replace(/[^0-9]/g, '')
    .padEnd(2, '0')
    .slice(0, 2)
  const cents = BigInt(`${whole}${frac}`)
  return formatUsdCents(negative && cents !== BigInt(0) ? `-${cents}` : cents, locale)
}

export function formatPrice(price: number): string {
  if (price === 0) return '$0.00'
  if (price >= 1) return `$${price.toFixed(2)}`
  if (price >= 0.01) return `$${price.toFixed(4)}`
  const str = price.toFixed(20)
  const match = str.match(/^0\.(0*)/)
  const leadingZeros = match ? match[1].length : 0
  return `$${price.toFixed(Math.min(leadingZeros + 4, 12))}`
}

export function formatCompactNumber(value: number): string {
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return value.toFixed(0)
}

/** Convert base units to a display number — lossy; avoid for settlement math. */
export function unitsToNumber(raw: string | bigint, decimals = 18): number {
  try {
    const n = typeof raw === 'string' ? BigInt(raw || '0') : raw
    const divisor = BigInt('1' + '0'.repeat(decimals))
    const whole = n / divisor
    const frac = n % divisor
    const fracDigits = frac.toString().padStart(decimals, '0').replace(/0+$/, '')
    return parseFloat(fracDigits ? `${whole}.${fracDigits}` : String(whole))
  } catch {
    return 0
  }
}
