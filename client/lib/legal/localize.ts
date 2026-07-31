const LEGAL_PATHS = /^(legal|terms|privacy|acceptable-use|cookies|risk-disclosure|dmca)(#.*)?$/

/** Prefix in-app legal links with the active locale segment. */
export function localizeLegalHtml(html: string, locale: string): string {
  return html.replace(/href="\/([^"]+)"/g, (match, path: string) => {
    if (path.startsWith('mailto:') || path.startsWith('http')) return match
    if (!LEGAL_PATHS.test(path)) return match
    return `href="/${locale}/${path}"`
  })
}
