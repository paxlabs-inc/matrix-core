import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'Cookie Policy',
  description: 'The cookies and local storage Matrix uses, and how to control them.',
}

export default async function CookiePolicyPage({
  params,
}: {
  params: Promise<{ locale: string }>
}) {
  const { locale } = await params
  return <LegalPage doc="cookies" locale={locale} />
}
