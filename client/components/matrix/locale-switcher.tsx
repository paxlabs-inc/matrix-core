'use client'

import { useLocale } from 'next-intl'
import { useTranslations } from 'next-intl'
import { Globe } from '@/lib/matrix-icons'
import { locales, localeLabels, type Locale } from '@/i18n/routing'
import { usePathname, useRouter } from '@/i18n/navigation'
import { DropdownMenu, DropdownMenuItem } from '@astryxdesign/core/DropdownMenu'

export function LocaleSwitcher() {
  const t = useTranslations('localeSwitcher')
  const currentLocale = useLocale() as Locale
  const pathname = usePathname()
  const router = useRouter()

  return (
    <DropdownMenu
      button={{
        label: localeLabels[currentLocale],
        icon: <Globe className="size-4" aria-hidden="true" />,
        tooltip: t('srLabel'),
        variant: 'ghost',
        size: 'sm',
      }}
      hasChevron
      placement="below"
    >
      {locales.map((locale) => (
        <DropdownMenuItem
          key={locale}
          label={localeLabels[locale]}
          endContent={locale === currentLocale ? <span aria-hidden>✓</span> : undefined}
          onClick={() => router.replace(pathname, { locale })}
        />
      ))}
    </DropdownMenu>
  )
}
