import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'Terms of Service',
  description: 'Terms of Service governing access to and use of Matrix, operated by PaxLabs Inc.',
}

export default async function TermsPage({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params
  return <LegalPage doc="terms" locale={locale} />
}
