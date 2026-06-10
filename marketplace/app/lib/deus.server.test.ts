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

describe("DeusClient dashboard endpoints (real Go contract)", () => {
  it("getService passes slugs through unencoded-safe (backend resolves id-or-slug)", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/services/alpha-weather");
      return json({
        id: "5b3f0c0e-0000-4000-8000-000000000001",
        slug: "alpha-weather",
        status: "active",
        kind: "data",
        mode: "proxy",
        display_name: "Alpha Weather",
        summary: "Forecasts",
        manifest_hash: "0xab",
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const svc = await c.getService("alpha-weather");
    expect(svc.id).toBe("5b3f0c0e-0000-4000-8000-000000000001");
    expect(svc.slug).toBe("alpha-weather");
  });

  it("parses catalog enrichment fields emitted by the Go handler", async () => {
    mockFetch(() =>
      json({
        services: [
          {
            id: "1", slug: "a", kind: "data", mode: "proxy", display_name: "A",
            summary: "s", status: "active", manifest_hash: "0x",
            price_wei: "100", unit: "request", tags: ["weather", "geo"],
          },
        ],
        total: 1, limit: 10, offset: 0,
      })
    );
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const cat = await c.catalog({ limit: 10 });
    expect(cat.services[0].price_wei).toBe("100");
    expect(cat.services[0].unit).toBe("request");
    expect(cat.services[0].tags).toEqual(["weather", "geo"]);
  });

  it("me() sends both caller and developer headers and parses MeResponse", async () => {
    let seen: Record<string, string> = {};
    mockFetch((url, init) => {
      expect(url).toBe("https://api.test/v1/me");
      seen = init.headers ?? {};
      return json({ did: "did:matrix:test:caller", wallet: "0xdev", display_name: "Alpha Dev" });
    });
    const c = new DeusClient({
      baseUrl: "https://api.test",
      caller: { did: "did:matrix:test:caller", bearer: "tok" },
      developer: { wallet: "0xdev" },
    });
    const me = await c.me();
    expect(seen["X-Caller-DID"]).toBe("did:matrix:test:caller");
    expect(seen["X-Developer-Wallet"]).toBe("0xdev");
    expect(me.display_name).toBe("Alpha Dev");
  });

  it("spend() parses totals + entries", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/me/spend");
      return json({
        total_spent_wei: "200",
        entries: [{ service_id: "1", display_name: "A", invocations: 2, total_wei: "200" }],
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", caller: { did: "did:x:y:z" } });
    const spend = await c.spend();
    expect(spend.total_spent_wei).toBe("200");
    expect(spend.entries[0].invocations).toBe(2);
  });

  it("myServices() unwraps {services} with usage aggregates", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/me/services");
      return json({
        services: [
          {
            id: "1", slug: "a", display_name: "A", status: "active", kind: "data",
            mode: "proxy", invocations: 3, revenue_wei: "300",
          },
        ],
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    const mine = await c.myServices();
    expect(mine).toHaveLength(1);
    expect(mine[0].revenue_wei).toBe("300");
  });

  it("earnings() parses totals, payout address, and settlement windows", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/me/earnings");
      return json({
        total_earned_wei: "300", pending_wei: "200", available_wei: "100",
        payout_address: "0xpay",
        settlements: [
          {
            id: "s1", window_start: "2026-06-01T00:00:00Z", window_end: "2026-06-08T00:00:00Z",
            amount_wei: "100", status: "pending",
          },
        ],
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    const earn = await c.earnings();
    expect(earn.available_wei).toBe("100");
    expect(earn.settlements[0].amount_wei).toBe("100");
  });

  it("analytics() parses series and top operations", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/services/svc-1/analytics");
      return json({
        service_id: "svc-1", total_invocations: 2, total_revenue_wei: "200",
        avg_latency_ms: 150, success_rate: 0.66, uptime_bps: 9990,
        series: [
          { date: "2026-06-09", invocations: 2, revenue_wei: "200", avg_latency_ms: 150, success_rate: 0.66 },
        ],
        top_operations: [{ operation: "forecast", invocations: 2, revenue_wei: "200" }],
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    const an = await c.analytics("svc-1");
    expect(an.total_invocations).toBe(2);
    expect(an.top_operations[0].operation).toBe("forecast");
  });

  it("logs() unwraps {logs} and degrades to [] on 404", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/services/svc-1/logs");
      return json({
        logs: [{ ts: "2026-06-09T00:00:00Z", level: "info", message: "invoke op=forecast units=1 latency=120ms outcome=ok" }],
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    const lines = await c.logs("svc-1");
    expect(lines).toHaveLength(1);
    expect(lines[0].level).toBe("info");

    mockFetch(() => json({ error: "not_found", message: "service not found" }, 404));
    expect(await c.logs("missing")).toEqual([]);
  });

  it("payout() POSTs the payout address and surfaces nothing_to_settle errors", async () => {
    mockFetch((url, init) => {
      expect(url).toBe("https://api.test/v1/services/svc-1/payout");
      const body = JSON.parse(String(init.body));
      expect(body.payout_address).toBe("0xpay");
      return json({ settlement_id: "set-1" });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    const res = await c.payout("svc-1", "0xpay");
    expect(res.settlement_id).toBe("set-1");

    mockFetch(() => json({ error: "nothing_to_settle", message: "no unsettled invocations to pay out yet" }, 409));
    try {
      await c.payout("svc-1", "0xpay");
      throw new Error("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(DeusApiError);
      expect((e as DeusApiError).code).toBe("nothing_to_settle");
    }
  });

  it("setServiceStatus() hits pause/delist and parses {id,status}", async () => {
    mockFetch((url) => {
      expect(url).toBe("https://api.test/v1/services/svc-1/pause");
      return json({ id: "svc-1", status: "paused" });
    });
    const c = new DeusClient({ baseUrl: "https://api.test", developer: { wallet: "0xdev" } });
    const res = await c.setServiceStatus("svc-1", "pause");
    expect(res.status).toBe("paused");
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

  it("attaches the SIWE developer token when present", async () => {
    let seen: Record<string, string> = {};
    mockFetch((_url, init) => {
      seen = init.headers ?? {};
      return json({ services: [] });
    });
    const c = new DeusClient({
      baseUrl: "https://api.test",
      developer: { wallet: "0xdev", token: "tok.abc" },
    });
    await c.myServices();
    expect(seen["X-Developer-Token"]).toBe("tok.abc");
    expect(seen["X-Developer-Wallet"]).toBe("0xdev");
  });
});

describe("developer auth (SIWE) endpoints", () => {
  it("developerNonce POSTs /v1/developers/nonce", async () => {
    mockFetch((url, init) => {
      expect(url).toBe("https://api.test/v1/developers/nonce");
      expect(init.method).toBe("POST");
      return json({ nonce: "n1", expires_at: "2026-06-10T00:05:00Z" });
    });
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const r = await c.developerNonce();
    expect(r.nonce).toBe("n1");
  });

  it("developerAuth POSTs message + signature and parses the token grant", async () => {
    mockFetch((url, init) => {
      expect(url).toBe("https://api.test/v1/developers/auth");
      const body = JSON.parse(String(init.body));
      expect(body.message).toContain("wants you to sign in");
      expect(body.signature).toMatch(/^0x/);
      return json({
        wallet: "0xabc0000000000000000000000000000000000abc",
        token: "tok.signed",
        expires_at: "2026-06-11T00:00:00Z",
      });
    });
    const c = new DeusClient({ baseUrl: "https://api.test" });
    const r = await c.developerAuth(
      "market.example wants you to sign in with your Ethereum account:\n0xabc0000000000000000000000000000000000abc\n\nNonce: n1",
      "0xdeadbeef"
    );
    expect(r.token).toBe("tok.signed");
  });

  it("developerAuth surfaces a 401 as DeusApiError", async () => {
    mockFetch(() => json({ error: "unauthorized", message: "signature does not match account" }, 401));
    const c = new DeusClient({ baseUrl: "https://api.test" });
    await expect(c.developerAuth("bad", "0x00")).rejects.toMatchObject({
      status: 401,
      code: "unauthorized",
    });
  });
});
