import { routing, type Locale } from '@/i18n/routing'

/** Strip `/en/...` → `{ locale: 'en', pathname: '/...' }`. */
export function stripLocalePrefix(pathname: string): { locale: Locale | null; pathname: string } {
  const segments = pathname.split('/').filter(Boolean)
  if (segments.length === 0) return { locale: null, pathname: '/' }

  const head = segments[0]
  if ((routing.locales as readonly string[]).includes(head)) {
    const rest = segments.slice(1).join('/')
    return { locale: head as Locale, pathname: rest ? `/${rest}` : '/' }
  }
  return { locale: null, pathname }
}

export function localePrefix(pathname: string, locale: Locale): string {
  const normalized = pathname.startsWith('/') ? pathname : `/${pathname}`
  if (normalized === '/') return `/${locale}`
  return `/${locale}${normalized}`
}

/** Strip a locale prefix from a same-origin href that may carry a query
 *  string: `/en/runs?x=1` → `/runs?x=1`, `/en?x=1` → `/?x=1`. */
export function stripLocalePrefixFromHref(href: string): string {
  const qi = href.indexOf('?')
  const path = qi === -1 ? href : href.slice(0, qi)
  const query = qi === -1 ? '' : href.slice(qi)
  return stripLocalePrefix(path).pathname + query
}
