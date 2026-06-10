#!/usr/bin/env node
/**
 * Local Deus `/v1` mock — a faithful, in-memory stand-in for the live Go
 * backend (https://deus.paxeer.app), used for local development and for
 * recorded browser evidence. The live prod catalog is intentionally empty and
 * must not be mutated, and a full local `deusd` needs Postgres + a chain; this
 * mock implements the exact wire contract the marketplace frontend consumes
 * (the real 22 routes + the dashboard read endpoints) with rich, deterministic
 * fixtures and mutable session state so create/deploy/publish flows work.
 *
 * Run: node mocks/deus-mock.mjs  (PORT defaults to 8787)
 */
import { createServer } from "node:http";
import { randomUUID } from "node:crypto";

const PORT = Number(process.env.MOCK_PORT || 8787);
const PAX = (n) => BigInt(Math.round(n * 1e6)).toString() + "0".repeat(12); // n PAX -> wei

let SERVICES = seedServices();
const INVOCATIONS = [];
const DEPLOYMENTS = new Map(); // serviceId -> deployment

function seedServices() {
  const mk = (o) => ({
    id: randomUUID(),
    status: "active",
    chain_id: 125,
    manifest_hash: "0x" + randomHex(64),
    created_at: new Date(Date.now() - o.ageDays * 864e5).toISOString(),
    invocations: o.invocations,
    revenue_wei: PAX(o.revenue),
    owned: !!o.owned,
    ...o,
  });
  return [
    mk({
      slug: "aether-weather", kind: "data", mode: "proxy", display_name: "Aether Weather Oracle",
      summary: "Hyper-local forecasts and historical climate series for any coordinate on Earth.",
      tags: ["weather", "geospatial", "forecast"], quality_score: "0.96", uptime_bps: 9995,
      price: 0.0008, unit: "request", invocations: 184203, revenue: 147.3, ageDays: 142,
      ops: [["forecast", "Point forecast up to 14 days", 0.0008], ["history", "Historical climate normals", 0.0012]],
    }),
    mk({
      slug: "lexica-translate", kind: "agent", mode: "hosted", display_name: "Lexica Translator",
      summary: "Context-aware translation across 94 languages with tone and glossary control.",
      tags: ["language", "translation", "nlp"], quality_score: "0.94", uptime_bps: 9982,
      price: 0.0021, unit: "1k tokens", invocations: 98211, revenue: 206.2, ageDays: 88,
      ops: [["translate", "Translate text preserving tone", 0.0021], ["detect", "Detect source language", 0.0003]],
    }),
    mk({
      slug: "sentinel-vision", kind: "agent", mode: "hosted", display_name: "Sentinel Vision",
      summary: "Object detection, OCR, and scene description for images and video frames.",
      tags: ["vision", "ocr", "multimodal"], quality_score: "0.91", uptime_bps: 9961,
      price: 0.004, unit: "image", invocations: 51288, revenue: 205.1, ageDays: 61,
      ops: [["detect", "Detect and label objects", 0.004], ["ocr", "Extract text from an image", 0.003], ["describe", "Natural-language scene caption", 0.005]],
    }),
    mk({
      slug: "ledger-quant", kind: "data", mode: "proxy", display_name: "Ledger Quant Feed",
      summary: "Real-time and historical on-chain market data, TVL, and token analytics.",
      tags: ["finance", "crypto", "market-data"], quality_score: "0.93", uptime_bps: 9990,
      price: 0.0005, unit: "query", invocations: 372910, revenue: 186.4, ageDays: 203, owned: true,
      ops: [["price", "Spot + TWAP price for a token", 0.0005], ["ohlc", "OHLC candles", 0.0009]],
    }),
    mk({
      slug: "scribe-summarize", kind: "agent", mode: "hosted", display_name: "Scribe Summarizer",
      summary: "Long-document summarization with citations and adjustable density.",
      tags: ["nlp", "summarization", "documents"], quality_score: "0.89", uptime_bps: 9947,
      price: 0.0015, unit: "1k tokens", invocations: 44120, revenue: 66.1, ageDays: 39, owned: true,
      ops: [["summarize", "Summarize a document", 0.0015], ["outline", "Generate a structured outline", 0.001]],
    }),
    mk({
      slug: "geocode-atlas", kind: "data", mode: "proxy", display_name: "Atlas Geocoder",
      summary: "Forward and reverse geocoding with administrative boundaries and timezones.",
      tags: ["geospatial", "maps", "geocoding"], quality_score: "0.95", uptime_bps: 9993,
      price: 0.0004, unit: "lookup", invocations: 211044, revenue: 84.4, ageDays: 121,
      ops: [["forward", "Address to coordinates", 0.0004], ["reverse", "Coordinates to address", 0.0004]],
    }),
    mk({
      slug: "muse-image", kind: "agent", mode: "hosted", display_name: "Muse Image Synth",
      summary: "Text-to-image generation tuned for product, brand, and editorial styles.",
      tags: ["vision", "generative", "image"], quality_score: "0.88", uptime_bps: 9925,
      price: 0.012, unit: "image", invocations: 28744, revenue: 344.9, ageDays: 47,
      ops: [["generate", "Generate an image from a prompt", 0.012], ["variation", "Create variations", 0.01]],
    }),
    mk({
      slug: "verity-kyc", kind: "data", mode: "proxy", display_name: "Verity Compliance", confidential: true,
      summary: "Privacy-preserving sanctions and risk screening for wallets and entities.",
      tags: ["compliance", "risk", "confidential"], quality_score: "0.97", uptime_bps: 9998,
      price: 0.006, unit: "screen", invocations: 19320, revenue: 115.9, ageDays: 75,
      ops: [["screen", "Screen an address or entity", 0.006]],
    }),
    mk({
      slug: "echo-speech", kind: "agent", mode: "hosted", display_name: "Echo Speech",
      summary: "Speech-to-text and text-to-speech with speaker diarization and 40 voices.",
      tags: ["audio", "speech", "transcription"], quality_score: "0.9", uptime_bps: 9956,
      price: 0.0018, unit: "minute", invocations: 63110, revenue: 113.6, ageDays: 54,
      ops: [["transcribe", "Audio to text with timestamps", 0.0018], ["synthesize", "Text to natural speech", 0.0016]],
    }),
    mk({
      slug: "graphml-recommend", kind: "agent", mode: "hosted", display_name: "GraphML Recommender",
      summary: "Real-time personalized recommendations from interaction graphs.",
      tags: ["ml", "recommendations", "graph"], quality_score: "0.87", uptime_bps: 9931,
      price: 0.0007, unit: "request", invocations: 154880, revenue: 108.4, ageDays: 33,
      ops: [["recommend", "Top-N recommendations", 0.0007], ["similar", "Similar items", 0.0006]],
    }),
    mk({
      slug: "forge-pdf", kind: "data", mode: "proxy", display_name: "Forge Document API",
      summary: "Render, merge, and extract structured data from PDFs and office files.",
      tags: ["documents", "pdf", "extraction"], quality_score: "0.92", uptime_bps: 9977,
      price: 0.0009, unit: "document", invocations: 87432, revenue: 78.7, ageDays: 98,
      ops: [["render", "HTML to PDF", 0.0009], ["extract", "Structured extraction", 0.0014]],
    }),
    mk({
      slug: "pulse-news", kind: "data", mode: "proxy", display_name: "Pulse News Stream",
      summary: "De-duplicated, classified global news with sentiment and entity tagging.",
      tags: ["news", "nlp", "realtime"], quality_score: "0.86", uptime_bps: 9919,
      price: 0.0003, unit: "query", invocations: 240551, revenue: 72.2, ageDays: 156,
      ops: [["search", "Search recent news", 0.0003], ["stream", "Subscribe to a topic", 0.0005]],
    }),
  ];
}

function toManifest(s) {
  return {
    schema_version: "2026-01",
    slug: s.slug, kind: s.kind, display_name: s.display_name, summary: s.summary,
    description:
      `${s.display_name} is a production-grade ${s.kind === "agent" ? "agent" : "data"} service on the Deus network. ` +
      `It exposes ${s.ops.length} metered operation${s.ops.length > 1 ? "s" : ""} with deterministic pricing and signed receipts. ` +
      `Calls are billed per ${s.ops[0] ? unitFor(s) : "request"} and settle natively in PAX.`,
    tags: s.tags, owner: "0x" + randomHex(40), payout_address: "0x" + randomHex(40),
    mode: s.mode, confidential: !!s.confidential,
    operations: s.ops.map(([name, desc, price]) => ({
      name, method: "POST", description: desc,
      input_schema: { type: "object", properties: { input: { type: "string", description: desc } }, required: ["input"] },
      output_schema: { type: "object", properties: { result: { type: "string" } } },
      timeout_ms: 30000, max_response_bytes: 262144,
    })),
    pricing: s.ops.map(([name, , price]) => ({
      operation: name, model: "per_unit", unit: unitFor(s), price_wei: PAX(price), min_charge_wei: PAX(price),
    })),
    ...(s.mode === "proxy" ? { endpoint: { proxy_url: `https://api.${s.slug}.example` } } : {}),
    sla: { target_uptime_bps: s.uptime_bps, p99_latency_ms: 220 },
  };
}
const unitFor = (s) => s.unit || "request";

function discoverResult(s) {
  return {
    id: s.id, slug: s.slug, display_name: s.display_name, summary: s.summary, kind: s.kind,
    quality_score: s.quality_score, uptime_bps: s.uptime_bps, score: scoreFor(s),
    operations: s.ops.map(([name, , price]) => ({ name, price_wei: PAX(price), unit: unitFor(s) })),
  };
}
const scoreFor = (s) => Math.min(1, (Number(s.quality_score) * 0.6 + (s.uptime_bps / 10000) * 0.4));

function catalogItem(s) {
  return {
    id: s.id, slug: s.slug, display_name: s.display_name, summary: s.summary, kind: s.kind,
    status: s.status, quality_score: s.quality_score, uptime_bps: s.uptime_bps,
    price_wei: PAX(s.price), unit: unitFor(s), tags: s.tags,
  };
}

function analyticsFor(s) {
  const days = 30;
  const series = [];
  let base = Math.max(40, Math.round(s.invocations / 220));
  for (let i = days - 1; i >= 0; i--) {
    const jitter = 0.75 + Math.sin(i / 3) * 0.15 + Math.random() * 0.2;
    const inv = Math.round(base * jitter);
    series.push({
      date: new Date(Date.now() - i * 864e5).toISOString().slice(0, 10),
      invocations: inv,
      revenue_wei: PAX(inv * s.price),
      avg_latency_ms: Math.round(160 + Math.random() * 120),
      success_rate: Number((0.985 + Math.random() * 0.014).toFixed(4)),
    });
  }
  return {
    service_id: s.id, total_invocations: s.invocations, total_revenue_wei: s.revenue_wei,
    avg_latency_ms: 198, success_rate: 0.992, uptime_bps: s.uptime_bps, series,
    top_operations: s.ops.map(([name], i) => ({
      operation: name, invocations: Math.round(s.invocations / (i + 1.5)),
      revenue_wei: PAX((s.price * s.invocations) / (i + 1.7)),
    })),
  };
}

function logsFor() {
  const now = Date.now();
  const lines = [
    ["info", "cold start: isolate booted in 7ms"],
    ["info", "npm install completed (14 packages)"],
    ["info", "deployment activated: src/main.js"],
    ["info", "handler ready, listening for executions"],
    ["info", "invoke op=forecast units=1 latency=182ms outcome=ok"],
    ["info", "invoke op=forecast units=1 latency=171ms outcome=ok"],
    ["warn", "upstream latency spike 540ms (recovered)"],
    ["info", "invoke op=history units=3 latency=205ms outcome=ok"],
  ];
  return lines.map((l, i) => ({
    ts: new Date(now - (lines.length - i) * 4200).toISOString(),
    level: l[0], message: l[1],
  }));
}

// ─── routing ──────────────────────────────────────────────────────────────
const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://localhost:${PORT}`);
  const path = url.pathname;
  const method = req.method || "GET";
  const send = (status, obj) => {
    res.writeHead(status, { "Content-Type": "application/json", "Access-Control-Allow-Origin": "*" });
    res.end(JSON.stringify(obj));
  };
  const err = (status, code, message) => send(status, { error: code, message });

  if (method === "OPTIONS") {
    res.writeHead(204, {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Methods": "GET,POST,OPTIONS",
      "Access-Control-Allow-Headers": "*",
    });
    return res.end();
  }

  try {
    if (path === "/internal/healthz") {
      return send(200, { ok: true, postgres: true, chain: true, version: "mock-1.0" });
    }

    if (path === "/v1/discover" && method === "POST") {
      const body = await readJson(req);
      return send(200, runDiscover(body.query || "", body.filters || {}, body.limit || 24));
    }
    if (path === "/v1/discover" && method === "GET") {
      return send(200, runDiscover(url.searchParams.get("query") || "", {
        kind: url.searchParams.get("kind") || "",
      }, 10));
    }

    // Mirrors the REAL Go handler (handlers_discovery.go handleCatalog):
    // limit/offset params only, {services,total,limit,offset} response,
    // and NO kind/query filtering (clients filter via /v1/discover).
    if (path === "/v1/catalog" && method === "GET") {
      const limit = Number(url.searchParams.get("limit") || 20);
      const offset = Number(url.searchParams.get("offset") || 0);
      const list = SERVICES.filter((s) => s.status === "active");
      const services = list.slice(offset, offset + limit).map(catalogItem);
      return send(200, { services, total: list.length, limit, offset });
    }

    // /v1/me/* (must precede /v1/services match patterns)
    if (path === "/v1/me" && method === "GET") {
      return send(200, {
        did: req.headers["x-caller-did"] || "did:matrix:marketplace:devuser",
        wallet: req.headers["x-caller-wallet"] || req.headers["x-developer-wallet"] || undefined,
        email: "developer@paxeer.app", display_name: "Developer",
      });
    }
    if (path === "/v1/me/spend" && method === "GET") {
      const owned = SERVICES.slice(0, 4);
      return send(200, {
        total_spent_wei: PAX(12.84),
        entries: owned.map((s) => ({
          service_id: s.id, display_name: s.display_name,
          invocations: Math.round(s.invocations / 90), total_wei: PAX(s.price * (s.invocations / 90)),
        })),
      });
    }
    if (path === "/v1/me/services" && method === "GET") {
      const mine = SERVICES.filter((s) => s.owned);
      return send(200, {
        services: mine.map((s) => ({
          id: s.id, slug: s.slug, display_name: s.display_name, status: s.status,
          kind: s.kind, mode: s.mode, invocations: s.invocations, revenue_wei: s.revenue_wei,
          uptime_bps: s.uptime_bps, quality_score: s.quality_score,
        })),
      });
    }
    if (path === "/v1/me/earnings" && method === "GET") {
      const mine = SERVICES.filter((s) => s.owned);
      const total = mine.reduce((a, s) => a + s.price * s.invocations, 0);
      return send(200, {
        total_earned_wei: PAX(total), pending_wei: PAX(total * 0.08), available_wei: PAX(total * 0.92),
        payout_address: "0x" + randomHex(40),
        settlements: Array.from({ length: 6 }).map((_, i) => ({
          id: randomUUID(), window_start: new Date(Date.now() - (i + 1) * 7 * 864e5).toISOString(),
          window_end: new Date(Date.now() - i * 7 * 864e5).toISOString(),
          amount_wei: PAX(total / 12 * (0.8 + Math.random() * 0.4)),
          status: i === 0 ? "pending" : "settled",
          tx_hash: i === 0 ? undefined : "0x" + randomHex(64),
        })),
      });
    }

    // /v1/services and sub-resources
    const svcMatch = path.match(/^\/v1\/services\/([^/]+)(\/(.*))?$/);
    if (path === "/v1/services" && method === "POST") {
      const body = await readJson(req);
      return send(200, createService(body.manifest || {}));
    }
    if (svcMatch) {
      const id = decodeURIComponent(svcMatch[1]);
      const sub = svcMatch[3] || "";
      const svc = SERVICES.find((s) => s.id === id || s.slug === id);
      if (!svc) return err(404, "not_found", "service not found");

      if (sub === "" && method === "GET") {
        return send(200, {
          id: svc.id, slug: svc.slug, status: svc.status, kind: svc.kind, mode: svc.mode,
          display_name: svc.display_name, summary: svc.summary, manifest_hash: svc.manifest_hash,
          chain_id: svc.chain_id, manifest: toManifest(svc),
        });
      }
      if (sub === "publish" && method === "POST") {
        svc.status = "active";
        return send(200, { id: svc.id, chain_id: 125, status: "active", manifest_hash: svc.manifest_hash, tx_hash: "0x" + randomHex(64) });
      }
      if ((sub === "pause" || sub === "delist") && method === "POST") {
        svc.status = sub === "pause" ? "paused" : "delisted";
        return send(200, { id: svc.id, status: svc.status });
      }
      if (sub === "artifacts" && method === "POST") {
        await drain(req);
        return send(200, { artifact_key: `artifacts/${svc.id}/${randomHex(8)}.tar.gz`, url: "" });
      }
      if (sub === "deploy" && method === "POST") {
        const body = await readJson(req).catch(() => ({}));
        const dep = {
          id: randomUUID(), service_id: svc.id, status: "deploying",
          runtime: body.runtime || "node20",
          exec_endpoint: `https://functions.paxeer.app/v1/functions/${randomHex(12)}/executions`,
          always_warm: !!body.always_warm, created_at: Date.now(),
        };
        DEPLOYMENTS.set(svc.id, dep);
        svc.mode = "hosted";
        return send(200, { deployment_id: dep.id, status: dep.status, exec_endpoint: dep.exec_endpoint, runtime: dep.runtime });
      }
      if (sub === "redeploy" && method === "POST") {
        const dep = DEPLOYMENTS.get(svc.id) || { id: randomUUID(), service_id: svc.id, runtime: "node20", always_warm: false };
        dep.status = "deploying"; dep.created_at = Date.now();
        DEPLOYMENTS.set(svc.id, dep);
        return send(200, { deployment_id: dep.id, status: dep.status, exec_endpoint: dep.exec_endpoint, runtime: dep.runtime });
      }
      const depMatch = sub.match(/^deployments\/([^/]+)$/);
      if (depMatch && method === "GET") {
        const dep = DEPLOYMENTS.get(svc.id);
        if (!dep) return err(404, "not_found", "deployment not found");
        // Simulate progression to active ~3s after deploy.
        if (dep.status === "deploying" && Date.now() - dep.created_at > 3000) dep.status = "active";
        return send(200, { id: dep.id, service_id: svc.id, status: dep.status, runtime: dep.runtime, exec_endpoint: dep.exec_endpoint, always_warm: dep.always_warm });
      }
      if (sub === "logs" && method === "GET") {
        return send(200, { logs: logsFor() });
      }
      if (sub === "analytics" && method === "GET") {
        return send(200, analyticsFor(svc));
      }
      if (sub === "payout" && method === "POST") {
        return send(200, { settlement_id: randomUUID() });
      }
    }

    if (path === "/v1/quote" || path.startsWith("/v1/quote/")) {
      const id = decodeURIComponent(path.slice("/v1/quote/".length));
      const svc = SERVICES.find((s) => s.id === id || s.slug === id);
      if (!svc) return err(404, "not_found", "service not found");
      const body = await readJson(req).catch(() => ({}));
      const op = svc.ops.find(([n]) => n === body.operation) || svc.ops[0];
      const units = body.estimated_units || "1";
      const unitWei = PAX(op[2]);
      return send(200, {
        quote_id: randomUUID(), service_id: svc.id, operation: op[0],
        unit_price_wei: unitWei, max_units: String(units),
        max_total_wei: (BigInt(unitWei) * BigInt(units)).toString(),
        pricing_version: 4, expires_at: new Date(Date.now() + 120000).toISOString(),
        eip712: { domain: "deus", digest: "0x" + randomHex(64), signature: "0x" + randomHex(130) },
      });
    }

    if (path.startsWith("/v1/invoke/")) {
      const id = decodeURIComponent(path.slice("/v1/invoke/".length));
      const svc = SERVICES.find((s) => s.id === id || s.slug === id);
      if (!svc) return err(404, "not_found", "service not found");
      if (!req.headers["x-caller-did"] && !req.headers["authorization"]) {
        return err(401, "unauthorized", "agent bearer required");
      }
      const body = await readJson(req).catch(() => ({}));
      const op = svc.ops.find(([n]) => n === body.operation) || svc.ops[0];
      const result = sampleResult(svc, op[0], body.args || {});
      const inv = {
        invocation_id: randomUUID(), outcome: "ok", result,
        charged_wei: PAX(op[2]), latency_ms: Math.round(150 + Math.random() * 120),
        receipt: { digest: "0x" + randomHex(64), gateway_sig: "0x" + randomHex(130), runner_sig: "0x" + randomHex(130), attestation: null },
        voucher: null,
      };
      INVOCATIONS.push({ service_id: svc.id, at: Date.now() });
      return send(200, inv);
    }

    return err(404, "not_found", `no route: ${method} ${path}`);
  } catch (e) {
    return err(500, "internal", String(e && e.message ? e.message : e));
  }
});

function runDiscover(query, filters, limit) {
  let list = SERVICES.filter((s) => s.status === "active");
  const q = (query || "").toLowerCase().trim();
  if (filters.kind) list = list.filter((s) => s.kind === filters.kind);
  if (filters.max_price_wei) list = list.filter((s) => BigInt(PAX(s.price)) <= BigInt(filters.max_price_wei));
  if (filters.min_uptime_bps) list = list.filter((s) => s.uptime_bps >= Number(filters.min_uptime_bps));
  if (filters.confidential === "true") list = list.filter((s) => s.confidential);
  if (q) list = list.filter((s) => matchText(s, q));
  list = list.sort((a, b) => scoreFor(b) - scoreFor(a)).slice(0, limit);
  return { results: list.map(discoverResult), next_cursor: null };
}

function matchText(s, q) {
  return (
    s.display_name.toLowerCase().includes(q) ||
    s.summary.toLowerCase().includes(q) ||
    s.slug.includes(q) ||
    (s.tags || []).some((t) => t.includes(q)) ||
    s.ops.some(([n, d]) => n.includes(q) || (d || "").toLowerCase().includes(q))
  );
}

function createService(manifest) {
  const slug = manifest.slug || `service-${randomHex(4)}`;
  const svc = {
    id: randomUUID(), slug, kind: manifest.kind || "data", mode: manifest.mode || "proxy",
    display_name: manifest.display_name || slug, summary: manifest.summary || "",
    tags: manifest.tags || [], status: "draft", chain_id: 125, manifest_hash: "0x" + randomHex(64),
    quality_score: "0", uptime_bps: 0, price: Number(weiToPax(manifest.pricing?.[0]?.price_wei)) || 0.001,
    unit: manifest.pricing?.[0]?.unit || "request", invocations: 0, revenue: 0, revenue_wei: PAX(0),
    owned: true, created_at: new Date().toISOString(),
    ops: (manifest.operations || [["run", "Run", 0.001]]).map((o) =>
      Array.isArray(o) ? o : [o.name, o.description || o.name, Number(weiToPax((manifest.pricing || []).find((p) => p.operation === o.name)?.price_wei)) || 0.001]
    ),
  };
  SERVICES = [svc, ...SERVICES];
  return {
    id: svc.id, slug: svc.slug, status: "draft", manifest_hash: svc.manifest_hash,
    validation: { ok: true, warnings: [] },
  };
}

function sampleResult(svc, op, args) {
  const input = args.input || args.message || args.text || args.query || "";
  switch (svc.slug) {
    case "aether-weather":
      return { location: input || "37.77,-122.42", forecast: [{ day: "Mon", high_c: 19, low_c: 12, summary: "Partly cloudy" }, { day: "Tue", high_c: 21, low_c: 13, summary: "Sunny" }], generated_at: new Date().toISOString() };
    case "lexica-translate":
      return { source_lang: "en", target_lang: args.target || "es", translation: input ? `«${input}» → traducción contextual` : "hola mundo" };
    case "ledger-quant":
      return { token: input || "PAX", price_usd: 1.42, change_24h: 0.031, tvl_usd: 18420000 };
    default:
      return { op, echo: input || "ok", note: `${svc.display_name} executed ${op}` };
  }
}

function weiToPax(wei) {
  if (!wei) return 0;
  try { return Number(BigInt(wei) / 1000000000000n) / 1e6; } catch { return 0; }
}

// ─── helpers ────────────────────────────────────────────────────────────────
function readJson(req) {
  return new Promise((resolve, reject) => {
    let data = "";
    req.on("data", (c) => (data += c));
    req.on("end", () => { try { resolve(data ? JSON.parse(data) : {}); } catch (e) { reject(e); } });
    req.on("error", reject);
  });
}
function drain(req) {
  return new Promise((resolve) => { req.on("data", () => {}); req.on("end", resolve); req.on("error", resolve); });
}
function randomHex(n) {
  const c = "0123456789abcdef";
  let s = "";
  for (let i = 0; i < n; i++) s += c[Math.floor(Math.random() * 16)];
  return s;
}

server.listen(PORT, () => {
  console.log(`[deus-mock] listening on http://localhost:${PORT} (${SERVICES.length} services seeded)`);
});
