import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'Privacy Policy',
  description:
    'How Matrix collects, uses, and protects personal data, and your rights under GDPR and CCPA/CPRA.',
}

export default async function PrivacyPage({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params
  return <LegalPage doc="privacy" locale={locale} />
}
