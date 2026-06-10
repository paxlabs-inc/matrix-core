import type { Route } from "./+types/healthz";
import { getEnv } from "@/lib/env";
import { resolveBaseUrl } from "@/lib/deus.server";

/**
 * Resource route: /healthz — liveness for uptime monitors. `?deep=1` also
 * probes the Deus backend (kept optional so the default check is free).
 */
export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);

  const body: Record<string, unknown> = {
    ok: true,
    service: "deus-marketplace",
    environment: env.ENVIRONMENT ?? "unknown",
  };

  if (url.searchParams.get("deep") === "1") {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 3_000);
    try {
      const res = await fetch(`${resolveBaseUrl(env)}/internal/healthz`, {
        signal: controller.signal,
      });
      body.deus = res.ok;
    } catch {
      body.deus = false;
    } finally {
      clearTimeout(timer);
    }
    body.ok = body.deus !== false;
  }

  return new Response(JSON.stringify(body), {
    status: body.ok ? 200 : 503,
    headers: {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    },
  });
}
