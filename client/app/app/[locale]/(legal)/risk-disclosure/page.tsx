import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'Risk Disclosure',
  description:
    'The material risks of using blockchain, AI agents, and the Paxeer Network. Not financial advice.',
}

export default async function RiskDisclosurePage({
  params,
}: {
  params: Promise<{ locale: string }>
}) {
  const { locale } = await params
  return <LegalPage doc="riskDisclosure" locale={locale} />
}
