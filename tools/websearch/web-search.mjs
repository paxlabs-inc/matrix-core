#!/usr/bin/env node
// web-search — MCP stdio bridge giving Matrix agents real internet search.
//
// Pairs with the baked-in `fetch` server (URL -> Markdown): web_search/web_news
// FIND sources, `fetch` READS them. Provider-agnostic — wraps Tavily (built for
// agents: ranked results + extracted content + optional synthesized answer) or
// the Brave Search API. Selected by WEBSEARCH_PROVIDER, else auto: Tavily if
// TAVILY_API_KEY is set, otherwise Brave.
//
// Keys are injected at the daemon process env (Q18 $env: refs in the manifest):
//   TAVILY_API_KEY   — https://tavily.com  (recommended)
//   BRAVE_API_KEY    — https://brave.com/search/api
// Optional: WEBSEARCH_PROVIDER=tavily|brave, WEBSEARCH_TIMEOUT_MS (default 15000),
//           WEBSEARCH_MAX_RESULTS cap (default 10).
//
// No API key required to BOOT: the server always starts and advertises its tools
// (so executor/mcp Manager.verifyTools passes); a missing key degrades to a
// structured "not configured" result only at call time.
//
// Wire protocol mirrors tools/paxeer/paxeer-net.mjs (newline-delimited JSON-RPC
// over stdio, zero npm deps, Node 18+ global fetch).
// Run `node tools/websearch/web-search.mjs --selftest` to smoke it offline.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const SERVER_NAME = 'web-search'
const SERVER_VERSION = '0.1.0'

const TIMEOUT_MS = clampInt(process.env.WEBSEARCH_TIMEOUT_MS, 15000, 1000, 120000)
const MAX_RESULTS_CAP = clampInt(process.env.WEBSEARCH_MAX_RESULTS, 10, 1, 20)
const MAX_RESPONSE_BYTES = clampInt(process.env.WEBSEARCH_MAX_RESPONSE_BYTES, 1_000_000, 10_000, 10_000_000)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

// ── provider selection ───────────────────────────────────────────────────────
function provider() {
  const forced = (process.env.WEBSEARCH_PROVIDER || '').trim().toLowerCase()
  if (forced === 'tavily') return process.env.TAVILY_API_KEY ? 'tavily' : 'none'
  if (forced === 'brave') return process.env.BRAVE_API_KEY ? 'brave' : 'none'
  if (process.env.TAVILY_API_KEY) return 'tavily'
  if (process.env.BRAVE_API_KEY) return 'brave'
  return 'none'
}

// ── result shaping ───────────────────────────────────────────────────────────
function ok(obj) {
  return { content: [{ type: 'text', text: typeof obj === 'string' ? obj : JSON.stringify(obj) }] }
}
function notConfigured(tool) {
  return {
    content: [{ type: 'text', text: JSON.stringify({
      ok: false, tool, error: 'web search not configured',
      hint: 'set TAVILY_API_KEY or BRAVE_API_KEY in the daemon env (Q18 $env: ref in the agent manifest)',
    }) }],
    isError: true,
  }
}

async function httpJson(method, url, { headers = {}, body } = {}) {
  if (typeof fetch !== 'function') throw new Error('web-search: global fetch unavailable (Node 18+ required)')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  let res
  try {
    res = await fetch(url, {
      method,
      headers: { Accept: 'application/json', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...headers },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : (e && e.message) || String(e)
    throw new Error(`web-search: ${method} ${url} failed: ${reason}`)
  }
  clearTimeout(timer)
  let raw = await res.text()
  if (raw.length > MAX_RESPONSE_BYTES) raw = raw.slice(0, MAX_RESPONSE_BYTES)
  let parsed = null
  try { parsed = raw ? JSON.parse(raw) : null } catch { /* non-JSON */ }
  if (!res.ok) {
    const m = parsed && (parsed.message || parsed.error || parsed.detail) ? parsed.message || parsed.error || parsed.detail : raw.slice(0, 300)
    throw new Error(`web-search: HTTP ${res.status} from ${hostOf(url)}: ${typeof m === 'string' ? m : JSON.stringify(m)}`)
  }
  return parsed
}
function hostOf(url) { try { return new URL(url).host } catch { return url } }
function clampResults(n) { return Math.min(MAX_RESULTS_CAP, Math.max(1, Number.parseInt(n ?? '', 10) || 5)) }

// ── Tavily ───────────────────────────────────────────────────────────────────
async function tavilySearch({ query, max_results, topic, include_answer }) {
  const data = await httpJson('POST', 'https://api.tavily.com/search', {
    headers: { Authorization: `Bearer ${process.env.TAVILY_API_KEY}` },
    body: {
      query,
      max_results: clampResults(max_results),
      topic: topic === 'news' ? 'news' : 'general',
      search_depth: 'basic',
      include_answer: include_answer !== false,
    },
  })
  const results = (data?.results || []).map((r) => ({
    title: r.title || null,
    url: r.url || null,
    snippet: typeof r.content === 'string' ? r.content.slice(0, 1200) : null,
    score: typeof r.score === 'number' ? r.score : undefined,
    published: r.published_date || undefined,
  }))
  return { provider: 'tavily', query, answer: data?.answer || undefined, results }
}

// ── Brave ────────────────────────────────────────────────────────────────────
async function braveSearch({ query, max_results, topic }) {
  const isNews = topic === 'news'
  const base = isNews
    ? 'https://api.search.brave.com/res/v1/news/search'
    : 'https://api.search.brave.com/res/v1/web/search'
  const url = `${base}?q=${encodeURIComponent(query)}&count=${clampResults(max_results)}`
  const data = await httpJson('GET', url, {
    headers: { 'X-Subscription-Token': process.env.BRAVE_API_KEY, 'Accept-Encoding': 'gzip' },
  })
  const rows = isNews ? (data?.results || []) : (data?.web?.results || [])
  const results = rows.map((r) => ({
    title: r.title || null,
    url: r.url || null,
    snippet: stripTags(r.description || r.snippet || '') || null,
    published: r.age || r.page_age || undefined,
  }))
  return { provider: 'brave', query, results }
}
function stripTags(s) { return String(s).replace(/<[^>]*>/g, '').slice(0, 1200) }

// ── dispatch ─────────────────────────────────────────────────────────────────
async function runSearch(tool, args, topic) {
  const query = (args?.query ?? '').toString().trim()
  if (!query) throw new Error('query is required')
  const p = provider()
  if (p === 'none') return notConfigured(tool)
  const opts = { query, max_results: args?.max_results, topic, include_answer: args?.include_answer }
  const out = p === 'tavily' ? await tavilySearch(opts) : await braveSearch(opts)
  return ok({ tool, ...out })
}

export async function dispatch(name, args = {}) {
  switch (name) {
    case 'web_search':
      return runSearch(name, args, args?.topic === 'news' ? 'news' : 'general')
    case 'web_news':
      return runSearch(name, args, 'news')
    default:
      throw new Error(`unknown tool: ${name}`)
  }
}

// ── tool descriptors (advertised to the MCP client; MUST match the manifest) ──
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })

export const tools = [
  {
    name: 'web_search',
    description: 'Search the public web for a query and return ranked results (title, url, snippet) plus an optional synthesized answer. Read-only. Pair with the `fetch` tool to read a result URL in full. args: query (required), max_results?, topic? ("general"|"news"), include_answer?',
    inputSchema: A({ query: S('search query'), max_results: N('1-20, default 5'), topic: S('"general" (default) or "news"'), include_answer: { type: 'boolean', description: 'synthesize a short answer (Tavily only); default true' } }, ['query']),
  },
  {
    name: 'web_news',
    description: 'Search recent news for a query and return ranked articles (title, url, snippet, published). Read-only. args: query (required), max_results?',
    inputSchema: A({ query: S('news query'), max_results: N('1-20, default 5') }, ['query']),
  },
]

export const TOOL_NAMES = tools.map((t) => t.name)

// ── JSON-RPC stdio server ─────────────────────────────────────────────────────
const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? '2024-11-05',
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    try {
      return await dispatch(name, args)
    } catch (err) {
      return { content: [{ type: 'text', text: JSON.stringify({ ok: false, tool: name, error: err?.message ?? String(err) }) }], isError: true }
    }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(obj) { process.stdout.write(JSON.stringify(obj) + '\n') }
const rpcOk = (id, result) => ({ jsonrpc: '2.0', id, result })
const rpcErr = (id, code, message) => ({ jsonrpc: '2.0', id, error: { code, message } })

function startStdioServer() {
  const rl = createInterface({ input: process.stdin })
  rl.on('line', async (line) => {
    if (!line.trim()) return
    let req
    try {
      req = JSON.parse(line)
    } catch (err) {
      send(rpcErr(null, -32700, 'parse error: ' + err.message))
      return
    }
    const fn = handlers[req.method]
    if (!fn) {
      if (req.id !== undefined) send(rpcErr(req.id, -32601, `method not found: ${req.method}`))
      return
    }
    try {
      const result = await fn(req.params)
      if (req.id !== undefined && result !== null) send(rpcOk(req.id, result))
    } catch (err) {
      if (req.id !== undefined) send(rpcErr(req.id, -32000, err?.message ?? String(err)))
    }
  })
  process.stdin.on('end', () => process.exit(0))
  process.on('SIGINT', () => process.exit(0))
  process.on('SIGTERM', () => process.exit(0))
}

// `--selftest`: list the registry, then verify it against every agent manifest
// that ships a web-search server. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time so it never reaches the
// fleet. Offline: reads only the local registry + agents/*.json (no network).
// WEBSEARCH_AGENTS_DIR overrides the manifest dir (used by tests).
function runSelftest() {
  console.log(`web-search: ${tools.length} tools (provider=${provider()})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.WEBSEARCH_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`web-search SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`web-search FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'web-search')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`web-search FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`web-search: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`web-search SELFTEST FAILED: no manifest under ${agentsDir} declares a web-search server`)
    process.exit(1)
  }
  if (drift) {
    console.error('web-search SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`web-search OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
