import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'Acceptable Use Policy',
  description:
    'Conduct that is prohibited on Matrix, including illicit-finance, abuse, and AI misuse.',
}

export default async function AcceptableUsePage({
  params,
}: {
  params: Promise<{ locale: string }>
}) {
  const { locale } = await params
  return <LegalPage doc="acceptableUse" locale={locale} />
}
