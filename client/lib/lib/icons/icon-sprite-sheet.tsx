import { readFileSync } from 'node:fs'
import { join } from 'node:path'

let cachedDefs: string | null = null

/** `<defs>…</defs>` block from the compiled paxscan sprite (server-only). */
export function getIconSpriteDefsHtml(): string {
  if (cachedDefs !== null) return cachedDefs
  try {
    const raw = readFileSync(join(process.cwd(), 'public/icons/sprite.svg'), 'utf8')
    cachedDefs = raw.match(/<defs>[\s\S]*<\/defs>/)?.[0] ?? ''
  } catch {
    cachedDefs = ''
  }
  return cachedDefs
}

/**
 * Inlines the paxscan icon sprite so `<use href="#id">` inherits `currentColor`
 * from the page. External `/icons/sprite.svg#id` references cannot see CSS color.
 */
export function IconSpriteSheet() {
  const defs = getIconSpriteDefsHtml()
  if (!defs) return null

  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden
      focusable="false"
      className="pointer-events-none absolute h-0 w-0 overflow-hidden"
      dangerouslySetInnerHTML={{ __html: defs }}
    />
  )
}
