import { getRequestConfig } from 'next-intl/server'
import { hasLocale } from 'next-intl'
import { routing } from './routing'
import enMessages from '../messages/en.json'

type Messages = Record<string, unknown>

function mergeMessages(base: Messages, override: Messages): Messages {
  const out: Messages = { ...base }
  for (const [key, value] of Object.entries(override)) {
    const prev = out[key]
    if (
      prev &&
      typeof prev === 'object' &&
      !Array.isArray(prev) &&
      value &&
      typeof value === 'object' &&
      !Array.isArray(value)
    ) {
      out[key] = mergeMessages(prev as Messages, value as Messages)
    } else {
      out[key] = value
    }
  }
  return out
}

export default getRequestConfig(async ({ requestLocale }) => {
  const requested = await requestLocale
  const locale = hasLocale(routing.locales, requested) ? requested : routing.defaultLocale

  const base = enMessages as Messages
  const messages =
    locale === routing.defaultLocale
      ? base
      : mergeMessages(base, (await import(`../messages/${locale}.json`)).default as Messages)

  return { locale, messages }
})
