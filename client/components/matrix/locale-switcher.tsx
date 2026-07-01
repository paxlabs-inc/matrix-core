'use client'

import { useLocale } from 'next-intl'
import { useTranslations } from 'next-intl'
import { Globe } from '@/lib/matrix-icons'
import { locales, localeLabels, type Locale } from '@/i18n/routing'
import { usePathname, useRouter } from '@/i18n/navigation'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Button } from '@/components/ui/button'

export function LocaleSwitcher() {
  const t = useTranslations('localeSwitcher')
  const currentLocale = useLocale() as Locale
  const pathname = usePathname()
  const router = useRouter()

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="sm"
          className="text-muted-foreground hover:text-foreground gap-2"
          aria-label={t('srLabel')}
        >
          <Globe className="size-4" aria-hidden="true" />
          <span className="text-xs">{localeLabels[currentLocale]}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {locales.map((locale) => (
          <DropdownMenuItem
            key={locale}
            onSelect={() => router.replace(pathname, { locale })}
            className={locale === currentLocale ? 'text-primary font-medium' : ''}
          >
            {localeLabels[locale]}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
