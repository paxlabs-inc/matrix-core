---
name: astro-site
description: Astro playbook for content/marketing/mostly-static sites — islands architecture, content collections, zero-JS by default, and selective hydration. Use when the app class is content/marketing.
origin: Matrix
---

# Astro Site Playbook

Doctrine pick for **content / marketing / mostly-static** sites (see
`rules/stack-selection/frontend.md`). Astro ships zero JavaScript by default and hydrates
only the islands you explicitly opt in — the best Core Web Vitals per unit of effort. A
marketing site built on a SPA framework ships a runtime to render text; Astro renders HTML.

Use when: building marketing pages, docs, blogs, landing pages, or any surface that is
mostly static content with a few interactive widgets.

## Init

```bash
npm create astro@latest my-site
cd my-site
npm run dev
```

Add integrations as needed:

```bash
npx astro add react      # or svelte / vue — for islands
npx astro add tailwind
npx astro add sitemap
```

## Idiomatic structure

```
src/
  pages/            # file-based routing; .astro or .md/.mdx → routes
    index.astro
    blog/[slug].astro
  content/
    blog/           # content collection entries (md/mdx)
    config.ts       # collection schemas (zod)
  components/       # .astro components (static) + framework islands
  layouts/          # shared page shells
public/             # static assets served as-is
```

## Content collections (typed content)

```ts
// src/content/config.ts
import { defineCollection, z } from "astro:content";
const blog = defineCollection({
  type: "content",
  schema: z.object({
    title: z.string(),
    pubDate: z.date(),
    draft: z.boolean().default(false),
  }),
});
export const collections = { blog };
```

```astro
---
// src/pages/blog/[slug].astro
import { getCollection } from "astro:content";
export async function getStaticPaths() {
  const posts = await getCollection("blog", ({ data }) => !data.draft);
  return posts.map(p => ({ params: { slug: p.slug }, props: { post: p } }));
}
const { post } = Astro.props;
const { Content } = await post.render();
---
<article><h1>{post.data.title}</h1><Content /></article>
```

Schemas give you typed, validated frontmatter — a build fails on a malformed post rather
than shipping it.

## Islands — hydrate only what is interactive

```astro
---
import Newsletter from "../components/Newsletter.jsx";
---
<Newsletter client:visible />   <!-- JS loads only when scrolled into view -->
```

Client directives, cheapest to most eager: `client:idle`, `client:visible`, `client:load`.
Default to `client:visible`. Everything without a directive is pure HTML with zero JS. Do
not make a whole page an island — that defeats Astro.

## Rendering modes

- **Static (default):** `output: 'static'` — prerender everything to HTML at build. Correct
  for the vast majority of content sites.
- **SSR/hybrid:** add an adapter (`@astrojs/node`, etc.) and `output: 'server'` only for the
  few dynamic routes (forms, personalization). Keep the rest static with
  `export const prerender = true`.

## Testing

- Content/build integrity: `astro check` (types + content schema) in CI.
- Component islands: **Vitest** + Testing Library for the framework you chose.
- E2E/visual: **Playwright** against the built output.
- Guard the reason you are here: run **Lighthouse**/CWV in CI and fail on JS-bloat
  regressions.

## Build and deploy

```bash
npm run build     # static HTML to dist/
npm run preview
```

Static `dist/` deploys to any CDN/static host. On Railway the detected start is a static
serve of `dist/`; if you added an SSR adapter it becomes a Node start — see the
`railway-deploy` skill.

## Common pitfalls

- Hydrating everything (`client:load` on all islands) — you rebuilt a SPA and lost the win.
- Choosing Astro for an app that is really an app (heavy interactivity, per-user state) —
  wrong class; that is a Next/React Router deviation, state it.
- Skipping content-collection schemas — you lose type safety and build-time validation.
- Putting secrets in client-visible code — SSR endpoints only for anything sensitive.
