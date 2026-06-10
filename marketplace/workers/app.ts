import { createRequestHandler } from "react-router";
import { requestContext } from "../app/lib/request-context.server";
import { captureException } from "../app/lib/sentry.server";

declare module "react-router" {
  export interface AppLoadContext {
    cloudflare: {
      env: Env;
      ctx: ExecutionContext;
    };
    /** Per-request CSP nonce, threaded to <Scripts>/<ServerRouter>. */
    cspNonce: string;
    /** Correlation ID, also forwarded to deusd as X-Request-ID. */
    requestId: string;
  }
}

const requestHandler = createRequestHandler(
  () => import("virtual:react-router/server-build"),
  import.meta.env.MODE
);

/**
 * CSRF defense-in-depth on top of SameSite=lax session cookies: browsers
 * always send Origin on cross-origin non-GET requests, so a mismatched
 * Origin (or `null` from sandboxed contexts) is rejected outright. Requests
 * without Origin/Referer (curl, server-to-server, MCP agents) pass through —
 * they carry no ambient cookie authority to abuse.
 */
function crossOriginViolation(request: Request): boolean {
  const method = request.method.toUpperCase();
  if (method === "GET" || method === "HEAD" || method === "OPTIONS") return false;

  const host = new URL(request.url).host;
  const origin = request.headers.get("Origin");
  if (origin) {
    if (origin === "null") return true;
    try {
      return new URL(origin).host !== host;
    } catch {
      return true;
    }
  }
  const referer = request.headers.get("Referer");
  if (referer) {
    try {
      return new URL(referer).host !== host;
    } catch {
      return true;
    }
  }
  return false;
}

function generateNonce(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function contentSecurityPolicy(nonce: string): string {
  return [
    "default-src 'self'",
    // strict-dynamic lets nonce'd module scripts load their imports; the
    // host fallbacks cover browsers that ignore strict-dynamic.
    // challenges.cloudflare.com = Turnstile (script + iframe + siteverify).
    `script-src 'self' 'nonce-${nonce}' 'strict-dynamic' https://challenges.cloudflare.com`,
    // React style attributes need unsafe-inline.
    "style-src 'self' 'unsafe-inline'",
    "font-src 'self'",
    "img-src 'self' data: https:",
    "connect-src 'self' https://challenges.cloudflare.com",
    "frame-src https://challenges.cloudflare.com",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    "upgrade-insecure-requests",
  ].join("; ");
}

/**
 * Baseline security headers on every response; CSP only on HTML documents and
 * only outside local dev (Vite HMR injects nonce-less inline scripts).
 */
function applySecurityHeaders(headers: Headers, nonce: string, production: boolean): void {
  headers.set("X-Content-Type-Options", "nosniff");
  headers.set("X-Frame-Options", "DENY");
  headers.set("Referrer-Policy", "strict-origin-when-cross-origin");
  headers.set("Cross-Origin-Opener-Policy", "same-origin");
  headers.set(
    "Permissions-Policy",
    "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
  );
  headers.set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload");
  const isHtml = (headers.get("Content-Type") ?? "").includes("text/html");
  if (isHtml && production) {
    headers.set("Content-Security-Policy", contentSecurityPolicy(nonce));
  }
}

// ─── Edge cache (anonymous public HTML) ─────────────────────────────────────
//
// Every public page view used to fan out to the single Go backend; at scale
// the backend, not Workers, is the bottleneck. Anonymous GETs of the public
// surfaces are cached at the PoP with manual stale-while-revalidate and
// stale-if-error. Anything carrying a session cookie bypasses the cache, and
// Set-Cookie responses are never stored.

const EDGE_FRESH_SECONDS = 60;
const EDGE_SWR_SECONDS = 600;
const EDGE_RETENTION_SECONDS = 86_400; // stale-if-error horizon
const CACHED_AT_HEADER = "X-Edge-Cached-At";
const ALLOWED_PARAMS = new Set(["q", "kind", "max_price", "min_uptime", "confidential", "page"]);

function cacheablePath(pathname: string): boolean {
  return (
    pathname === "/" ||
    pathname === "/catalog" ||
    pathname === "/discover" ||
    /^\/services\/[^/]+$/.test(pathname)
  );
}

function hasSessionCookie(request: Request): boolean {
  return (request.headers.get("Cookie") ?? "").includes("deus_session=");
}

/** Stable cache key: whitelisted params only, sorted (tracking junk dropped). */
function normalizedCacheKey(request: Request): Request {
  const url = new URL(request.url);
  const kept = [...url.searchParams.entries()]
    .filter(([k]) => ALLOWED_PARAMS.has(k))
    .sort(([a], [b]) => a.localeCompare(b));
  url.search = new URLSearchParams(kept).toString();
  return new Request(url.toString(), { method: "GET" });
}

function cachedAgeSeconds(response: Response): number {
  const at = Number(response.headers.get(CACHED_AT_HEADER) ?? 0);
  if (!at) return Number.POSITIVE_INFINITY;
  return (Date.now() - at) / 1000;
}

function storableCopy(response: Response): Response {
  const copy = new Response(response.body, response);
  copy.headers.set(CACHED_AT_HEADER, String(Date.now()));
  // s-maxage governs Cache API retention; our own freshness logic runs off
  // the timestamp header so we can serve-stale long past EDGE_FRESH_SECONDS.
  copy.headers.set("Cache-Control", `public, s-maxage=${EDGE_RETENTION_SECONDS}`);
  return copy;
}

function clientCopy(response: Response, edgeStatus: string): Response {
  const copy = new Response(response.body, response);
  copy.headers.set("X-Edge-Cache", edgeStatus);
  // Browsers always revalidate; the shared edge cache does the heavy lifting.
  copy.headers.set("Cache-Control", "public, max-age=0, must-revalidate");
  return copy;
}

export default {
  async fetch(request, env, ctx) {
    if (crossOriginViolation(request)) {
      return new Response("Cross-origin request rejected", { status: 403 });
    }

    const requestId = crypto.randomUUID();
    const startedAt = Date.now();
    const url = new URL(request.url);

    const logRequest = (status: number, edgeCache?: string) => {
      // One structured JSON line per request — Workers Logs indexes fields.
      console.log(
        JSON.stringify({
          msg: "request",
          request_id: requestId,
          method: request.method,
          path: url.pathname,
          status,
          duration_ms: Date.now() - startedAt,
          edge_cache: edgeCache,
        })
      );
    };

    const respond = async (): Promise<Response> => {
      const cspNonce = generateNonce();
      try {
        const response = await requestContext.run({ requestId }, () =>
          requestHandler(request, {
            cloudflare: { env, ctx },
            cspNonce,
            requestId,
          })
        );
        const out = new Response(response.body, response);
        applySecurityHeaders(out.headers, cspNonce, env.ENVIRONMENT !== "development");
        out.headers.set("X-Request-ID", requestId);
        return out;
      } catch (err) {
        ctx.waitUntil(captureException(env, err, { requestId, url: request.url }));
        throw err;
      }
    };

    const cacheEligible =
      request.method === "GET" &&
      env.ENVIRONMENT !== "development" &&
      cacheablePath(url.pathname) &&
      !hasSessionCookie(request);

    if (!cacheEligible) {
      const response = await respond();
      logRequest(response.status);
      return response;
    }

    // Workers-runtime CacheStorage exposes `.default`; the DOM lib type wins
    // in tsconfig, so narrow explicitly.
    const cache = (caches as unknown as { default: Cache }).default;
    const cacheKey = normalizedCacheKey(request);
    const cached = await cache.match(cacheKey);

    const refresh = async (): Promise<Response> => {
      const fresh = await respond();
      if (fresh.ok && !fresh.headers.has("Set-Cookie")) {
        await cache.put(cacheKey, storableCopy(fresh.clone()));
      }
      return fresh;
    };

    if (cached) {
      const age = cachedAgeSeconds(cached);
      if (age < EDGE_FRESH_SECONDS) {
        logRequest(cached.status, "hit");
        return clientCopy(cached, "hit");
      }
      if (age < EDGE_SWR_SECONDS) {
        ctx.waitUntil(refresh().then((r) => r.body?.cancel()));
        logRequest(cached.status, "stale-while-revalidate");
        return clientCopy(cached, "stale-while-revalidate");
      }
    }

    const fresh = await refresh();
    if (fresh.status >= 500 && cached) {
      // stale-if-error: a backend blip must not surface as a 5xx.
      logRequest(cached.status, "stale-if-error");
      return clientCopy(cached, "stale-if-error");
    }
    logRequest(fresh.status, cached ? "expired" : "miss");
    return clientCopy(fresh, cached ? "expired" : "miss");
  },
} satisfies ExportedHandler<Env>;
