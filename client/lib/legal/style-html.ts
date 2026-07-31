import { localizeLegalHtml } from '@/lib/legal/localize'

/** Map static legal HTML class names to Matrix app Tailwind utilities. */
const CLASS_MAP: Record<string, string> = {
  shell:
    'mx-auto grid max-w-6xl gap-10 px-6 pb-20 pt-8 lg:grid-cols-[minmax(0,220px)_1fr] lg:gap-14',
  toc: 'hidden lg:block sticky top-24 self-start text-sm text-muted-foreground [&_h4]:mb-3 [&_h4]:text-xs [&_h4]:font-semibold [&_h4]:uppercase [&_h4]:tracking-wider [&_ol]:list-none [&_ol]:space-y-1 [&_ol]:p-0 [&_a]:block [&_a]:rounded-md [&_a]:py-1 [&_a]:text-muted-foreground hover:[&_a]:text-foreground',
  doc: 'min-w-0 max-w-3xl space-y-4 [&_h1]:text-3xl [&_h1]:font-semibold [&_h1]:tracking-tight [&_h2]:mt-10 [&_h2]:flex [&_h2]:scroll-mt-24 [&_h2]:items-baseline [&_h2]:gap-3 [&_h2]:border-t [&_h2]:border-border [&_h2]:pt-8 [&_h2]:text-xl [&_h2]:font-semibold [&_h3]:mt-6 [&_h3]:text-base [&_h3]:font-medium [&_p]:leading-relaxed [&_p]:text-muted-foreground [&_ul]:space-y-2 [&_ul]:pl-5 [&_ul]:text-muted-foreground [&_li]:leading-relaxed [&_a]:text-primary [&_a]:underline-offset-2 hover:[&_a]:underline [&_strong]:font-semibold [&_strong]:text-foreground [&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-xs',
  eyebrow:
    'bg-primary/10 text-primary mb-4 inline-flex items-center gap-2 rounded-full px-3 py-1 text-xs font-semibold uppercase tracking-wider',
  dot: 'bg-primary size-1.5 rounded-full',
  lede: 'text-muted-foreground max-w-2xl text-base leading-relaxed',
  meta: 'text-muted-foreground flex flex-wrap gap-x-5 gap-y-2 border-t border-border pt-4 text-sm',
  callout:
    'border-destructive/30 bg-destructive/10 text-foreground my-6 rounded-lg border p-4 text-sm leading-relaxed [&_.label]:text-destructive [&_.label]:mb-2 [&_.label]:block [&_.label]:text-xs [&_.label]:font-semibold [&_.label]:uppercase [&_.label]:tracking-wider',
  note: 'border-primary/20 bg-primary/5 text-foreground my-6 rounded-lg border p-4 text-sm leading-relaxed [&_.label]:text-primary [&_.label]:mb-2 [&_.label]:block [&_.label]:text-xs [&_.label]:font-semibold [&_.label]:uppercase [&_.label]:tracking-wider',
  num: 'text-primary font-mono text-xs font-medium',
  'tbl-wrap':
    'border-border my-6 overflow-x-auto rounded-lg border [&_table]:w-full [&_table]:text-sm [&_th]:bg-muted [&_th]:px-4 [&_th]:py-3 [&_th]:text-left [&_th]:font-medium [&_td]:border-t [&_td]:border-border [&_td]:px-4 [&_td]:py-3 [&_td]:align-top',
  grid: 'my-8 grid gap-4 sm:grid-cols-2',
  card: 'bg-card text-card-foreground hover:border-primary/30 group flex flex-col gap-2 rounded-xl border p-5 shadow-sm transition-colors hover:bg-accent/40 no-underline',
  kicker: 'text-primary font-mono text-xs',
  go: 'text-primary mt-auto text-sm font-medium group-hover:underline',
  foot: 'border-border bg-card mt-12 border-t',
  'foot-inner':
    'text-muted-foreground mx-auto flex max-w-6xl flex-wrap items-center justify-between gap-6 px-6 py-10 text-sm',
  links:
    'flex flex-wrap gap-4 [&_a]:text-muted-foreground hover:[&_a]:text-foreground [&_a]:no-underline',
  'entity-table': '',
}

function mapClassAttribute(classValue: string): string {
  const mapped = classValue
    .split(/\s+/)
    .filter(Boolean)
    .map((token) => CLASS_MAP[token] ?? '')
    .join(' ')
    .trim()
  return mapped || classValue
}

export function styleLegalHtml(html: string, locale: string): string {
  let out = localizeLegalHtml(html, locale)
  out = out.replace(/\s+style="[^"]*"/g, '')
  out = out.replace(/class="([^"]*)"/g, (_match, classes: string) => {
    return `class="${mapClassAttribute(classes)}"`
  })
  return out
}
