import type { Metadata } from 'next'
import { LegalPage } from '@/components/legal/legal-page'

export const metadata: Metadata = {
  title: 'DMCA Notice',
  description: 'DMCA Copyright Policy and designated agent notice for Centra AI.',
}

export default async function DmcaPage({ params }: { params: Promise<{ locale: string }> }) {
  const { locale } = await params
  return <LegalPage doc="dmca" locale={locale} />
}
