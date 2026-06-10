import type { Route } from "./+types/sitemap";
import { getEnv } from "@/lib/env";
import { createDeusClient } from "@/lib/deus.server";
import { cachedJson } from "@/lib/cache.server";

function xmlEscape(s: string): string {
  return s
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

/** Resource route: /sitemap.xml — static surfaces + every active listing. */
export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const origin = new URL(request.url).origin;
  const deus = createDeusClient(env);

  let slugs: string[] = [];
  try {
    const cat = await cachedJson(env, "sitemap:catalog", 600, () => deus.catalog({ limit: 100 }));
    slugs = cat.services.map((s) => s.slug).filter(Boolean);
  } catch {
    // Backend down: still serve the static surfaces.
  }

  const urls = [
    `${origin}/`,
    `${origin}/catalog`,
    `${origin}/discover`,
    ...slugs.map((slug) => `${origin}/services/${encodeURIComponent(slug)}`),
  ];

  const body =
    `<?xml version="1.0" encoding="UTF-8"?>\n` +
    `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n` +
    urls.map((u) => `  <url><loc>${xmlEscape(u)}</loc></url>`).join("\n") +
    `\n</urlset>\n`;

  return new Response(body, {
    headers: {
      "Content-Type": "application/xml; charset=utf-8",
      "Cache-Control": "public, max-age=3600",
    },
  });
}
