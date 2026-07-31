import { defineRouting } from 'next-intl/routing'

export const routing = defineRouting({
  locales: ['en', 'de', 'es', 'ja', 'zh-CN'],
  defaultLocale: 'en',
  /** Every locale in the URL — shareable, restorable links. */
  localePrefix: 'always',
})

export type Locale = (typeof routing.locales)[number]
export const locales = routing.locales
export const defaultLocale = routing.defaultLocale

export const localeLabels: Record<Locale, string> = {
  en: 'English',
  de: 'Deutsch',
  es: 'Español',
  ja: '日本語',
  'zh-CN': '中文',
}
