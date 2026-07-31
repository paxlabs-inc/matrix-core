/**
 * HTML sanitization for trusted-first-party SVG snippets inlined via
 * dangerouslySetInnerHTML. Strips scripts, event handlers, and foreign
 * object/embed tags.
 */
import DOMPurify from 'isomorphic-dompurify'

const SVG_PROFILE = {
  USE_PROFILES: { svg: true, svgFilters: true },
  ADD_TAGS: ['use'],
  ADD_ATTR: ['href', 'xlink:href', 'viewBox', 'xmlns', 'fill', 'stroke', 'stroke-width'],
}

export function sanitizeSvgMarkup(html: string): string {
  const trimmed = html.trim()
  if (!trimmed) return ''
  // DOMPurify drops bare <path>/<circle> fragments; wrap so the SVG profile applies.
  const wrapped = `<svg xmlns="http://www.w3.org/2000/svg">${trimmed}</svg>`
  const clean = DOMPurify.sanitize(wrapped, SVG_PROFILE)
  return clean.replace(/^<svg[^>]*>/, '').replace(/<\/svg>$/, '')
}

export function sanitizeHtml(html: string): string {
  return DOMPurify.sanitize(html)
}
