import type { Metadata, Viewport } from 'next'
import localFont from 'next/font/local'
import { Suspense } from 'react'
import { notFound } from 'next/navigation'
import { NextIntlClientProvider } from 'next-intl'
import { getMessages, setRequestLocale } from 'next-intl/server'
import { hasLocale } from 'next-intl'
import { AppProviders } from '@/components/providers/AppProviders'
import { WebVitalsReporter } from '@/components/observability/web-vitals-reporter'
import { NetworkStatus } from '@/components/observability/network-status'
import { CookieConsent } from '@/components/compliance/cookie-consent'
import { VercelAnalytics } from '@/components/observability/vercel-analytics'
import { SquiCircleFilter } from '@/components/matrix/squi-circle-filter'
import { routing } from '@/i18n/routing'
import { BRAND_DESCRIPTION, BRAND_NAME } from '@/lib/brand'
import Loading from './loading'
import '../globals.css'

const matrixSans = localFont({
  src: [
    {
      path: '../../public/matrix-font/LTWaveText-Regular.otf',
      weight: '400',
      style: 'normal',
    },
    { path: '../../public/matrix-font/LTWaveText-Medium.otf', weight: '500', style: 'normal' },
    // LT Wave ships no Semibold cut: Bold covers both 600 and 700 so
    // font-semibold and font-bold stay visually distinct from Medium.
    { path: '../../public/matrix-font/LTWaveText-Bold.otf', weight: '600 700', style: 'normal' },
  ],
  variable: '--font-matrix-sans',
  display: 'optional',
  preload: false,
})

const matrixMono = localFont({
  src: '../../public/matrix-font/LTWaveMono-Regular.otf',
  variable: '--font-matrix-mono',
  display: 'optional',
  preload: false,
})

export const metadata: Metadata = {
  title: {
    default: `${BRAND_NAME} — agent operations`,
    template: `%s | ${BRAND_NAME}`,
  },
  description: `${BRAND_DESCRIPTION} ${BRAND_NAME} optimizes for completion, not conversation.`,
  applicationName: BRAND_NAME,
  icons: {
    icon: [{ url: '/centra-icon.svg', type: 'image/svg+xml' }],
    shortcut: '/centra-icon.svg',
    apple: '/centra-icon.svg',
  },
  robots: { index: false, follow: false },
}

/** All locale routes are authenticated / cookie-dependent — never prerender. */
export const dynamic = 'force-dynamic'

export const viewport: Viewport = {
  themeColor: '#0a0a0b',
  colorScheme: 'dark',
  width: 'device-width',
  initialScale: 1,
}

export function generateStaticParams() {
  return routing.locales.map((locale) => ({ locale }))
}

export default async function LocaleLayout({
  children,
  params,
}: {
  children: React.ReactNode
  params: Promise<{ locale: string }>
}) {
  const { locale } = await params
  if (!hasLocale(routing.locales, locale)) notFound()

  setRequestLocale(locale)
  const messages = await getMessages()

  return (
    <html
      lang={locale}
      data-theme="dark"
      className={`${matrixSans.variable} ${matrixMono.variable} dark bg-background`}
      suppressHydrationWarning
    >
      <body className="font-sans antialiased">
        <a href="#main-content" className="skip-link">
          Skip to content
        </a>
        <SquiCircleFilter />
        <NextIntlClientProvider locale={locale} messages={messages}>
          <NetworkStatus />
          <AppProviders locale={locale}>
            <Suspense fallback={<Loading />}>{children}</Suspense>
            <WebVitalsReporter />
            <CookieConsent />
          </AppProviders>
        </NextIntlClientProvider>
        <VercelAnalytics />
      </body>
    </html>
  )
}
