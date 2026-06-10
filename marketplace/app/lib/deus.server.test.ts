import { afterEach, describe, expect, it, vi } from "vitest";
import { DeusApiError, DeusClient, resolveBaseUrl } from "./deus.server";

type Handler = (url: string, init: { method?: string; body?: unknown; headers?: Record<string, string> }) => Response;

function mockFetch(handler: Handler) {
  globalThis.fetch = vi.fn(async (url: unknown, init: unknown) =>
    handler(String(url), (init ?? {}) as Parameters<Handler>[1])
  ) as unknown as typeof fetch;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("resolveBaseUrl", () => {
  it("defaults to prod and strips trailing slashes", () => {
    expect(resolveBaseUrl(undefined)).toBe("https://deus.paxeer.app");
    expect(resolveBaseUrl({ DEUS_API_URL: "http://localhost:8787/" })).toBe("http://localhost:8787");
  });
});

describe("DeusClient.discover", () => {
  it("POSTs query + filters and returns results", async () => {
    mockFetch((url, init) => {
      expect(url).toBe("https://api.test/v1/discover");
      expect(init.method).toBe("POST");
      const body = JSON.parse(String(init.body));
      expect(body.query).toBe("weather");
      expect(body.limit).toBe(24);
      return json({
        results: [
          { id: "1", slug: "a", display_name: "A", summary: "", kind: "data", score: 1, operations: [] },
        ],
        next_cursor: null,
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const r = await c.discover({ query: "weather" });
    expect(r.results).toHaveLength(1);
  });
});

describe("DeusClient.catalog", () => {
  it("GETs /v1/catalog with limit/offset and returns the real Go shape", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/catalog?limit=24&offset=12");
      return json({
        services: [{ id: "1", slug: "a", display_name: "A", summary: "", kind: "data", status: "active" }],
        total: 13,
        limit: 24,
        offset: 12,
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const cat = await c.catalog({ limit: 24, offset: 12 });
    expect(cat.services).toHaveLength(1);
    expect(cat.total).toBe(13);
  });

  it("routes kind-filtered browsing through discover (Go catalog cannot filter)", async () => {
    mockFetch((url, init) => {
      expect(url).toBe("https://api.test/v1/discover");
      const body = JSON.parse(String(init.body));
      expect(body.filters.kind).toBe("data");
      return json({
        results: [
          {
            id: "1", slug: "a", display_name: "A", summary: "s", kind: "data", score: 1,
            quality_score: "0.9", uptime_bps: 9990,
            operations: [{ name: "op", price_wei: "100", unit: "req" }],
          },
        ],
        next_cursor: null,
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const cat = await c.catalog({ limit: 10, kind: "data" });
    expect(cat.services).toHaveLength(1);
    expect(cat.services[0].price_wei).toBe("100");
    expect(cat.services[0].unit).toBe("req");
  });
});

describe("error handling", () => {
  it("throws a typed DeusApiError carrying status + code", async () => {
    mockFetch(() => json({ error: "forbidden", message: "not your service" }, 403));
    const c = new DeusClient({ baseUrl: "https://api.test" });
    await expect(c.getService("x")).rejects.toBeInstanceOf(DeusApiError);
    try {
      await c.getService("x");
      throw new Error("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(DeusApiError);
      const err = e as DeusApiError;
      expect(err.status).toBe(403);
      expect(err.code).toBe("forbidden");
      expect(err.message).toBe("not your service");
    }
  });
});

describe("auth header injection", () => {
  it("attaches caller headers to quote", async () => {
    let seen: Record<string, string> = {};
    mockFetch((_url, init) => {
      seen = init.headers ?? {};
      return json({ quote_id: "q", service_id: "svc" });
    });
    const c = new DeusClient({
      baseUrl: "https://api.test",
      caller: { bearer: "tok", did: "did:matrix:marketplace:abc", wallet: "0xabc" },
    });
    await c.quote("svc", { operation: "op", estimated_units: "1" });
    expect(seen["Authorization"]).toBe("Bearer tok");
    expect(seen["X-Caller-DID"]).toBe("did:matrix:marketplace:abc");
    expect(seen["X-Caller-Wallet"]).toBe("0xabc");
  });

  it("attaches developer headers to createService", async () => {
    let seen: Record<string, string> = {};
    mockFetch((_url, init) => {
      seen = init.headers ?? {};
      return json({ id: "1", slug: "s", status: "draft", manifest_hash: "0x", validation: { ok: true, warnings: [] } });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    await c.createService({ slug: "s" });
    expect(seen["X-Developer-Wallet"]).toBe("0xdev");
  });
});
