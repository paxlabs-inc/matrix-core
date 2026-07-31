import { styleLegalHtml } from '@/lib/legal/style-html'
import type { LegalDocKey } from '@/lib/legal/documents'
import { legalDocuments } from '@/lib/legal/documents'
import { LegalShell } from '@/components/legal/legal-shell'

type LegalPageProps = {
  doc: LegalDocKey
  locale: string
}

export function LegalPage({ doc, locale }: LegalPageProps) {
  const { main, footer } = legalDocuments[doc]
  const mainHtml = styleLegalHtml(main, locale)
  const footerHtml = styleLegalHtml(footer, locale)

  return (
    <LegalShell>
      <div id="main-content" dangerouslySetInnerHTML={{ __html: mainHtml }} />
      <div dangerouslySetInnerHTML={{ __html: footerHtml }} />
    </LegalShell>
  )
}
