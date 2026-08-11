import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'Legal',
  description:
    'Legal and compliance center for Centra AI, the cognition and intent layer of the Paxeer Network.',
}

export default async function LegalHomePage({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params
  return <LegalPage doc="index" locale={locale} />
}
