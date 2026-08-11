#!/usr/bin/env node
// searxng — MCP stdio bridge giving Centra AI agents private web search via a
// SELF-HOSTED SearXNG meta-search instance (no API keys, no per-query bills).
//
// Pairs with the baked-in `fetch` server (URL -> Markdown) and `firecrawl`
// (bulk scrape/crawl): searxng_search FINDS sources, `fetch`/`firecrawl` READ
// them. Same shape as tools/browser/browser.mjs + tools/tachyon/tachyon.mjs:
// the daemon spawns this over stdio (the transport executor/mcp/http.go
// supports) and it forwards each search to the remote SearXNG JSON API at
// MATRIX_SEARXNG_URL (GET /search?format=json).
//
// Boot decoupling: initialize/tools/list are answered LOCALLY, so an
// unreachable SearXNG never bricks daemon boot (executor spawn failures are
// fatal) — searxng_search just returns a structured error until it is
// reachable.
//
// Remote endpoint: MATRIX_SEARXNG_URL (e.g. https://searxng-...up.railway.app).
// Optional auth:   MATRIX_SEARXNG_TOKEN -> `Authorization: Bearer <tok>` (only
//                  if the instance is fronted by a bearer gate; SearXNG itself
//                  needs none).
// Optional:        MATRIX_SEARXNG_TIMEOUT_MS (default 20000),
//                  MATRIX_SEARXNG_MAX_RESULTS cap (default 10).
//
// Wire protocol mirrors tools/websearch/web-search.mjs (newline-delimited
// JSON-RPC over stdio, zero npm deps, Node 18+ global fetch).
// Run `node tools/searxng/searxng.mjs --selftest` to smoke it offline.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const SERVER_NAME = 'searxng'
const SERVER_VERSION = '0.1.0'
const PROTOCOL_VERSION = '2024-11-05'

const REMOTE_URL = (process.env.MATRIX_SEARXNG_URL || '').trim().replace(/\/+$/, '')
const REMOTE_TOKEN = (process.env.MATRIX_SEARXNG_TOKEN || '').trim()
const TIMEOUT_MS = clampInt(process.env.MATRIX_SEARXNG_TIMEOUT_MS, 20000, 2000, 120000)
const MAX_RESULTS_CAP = clampInt(process.env.MATRIX_SEARXNG_MAX_RESULTS, 10, 1, 50)
const MAX_RESPONSE_BYTES = clampInt(process.env.MATRIX_SEARXNG_MAX_RESPONSE_BYTES, 2_000_000, 10_000, 20_000_000)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

// ── result shaping ───────────────────────────────────────────────────────────
function okResult(obj) {
  return { content: [{ type: 'text', text: JSON.stringify(obj) }], isError: false }
}
function errResult(tool, error, extra = {}) {
  return {
    content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }],
    isError: true,
  }
}

function hostOf(url) { try { return new URL(url).host } catch { return url } }

async function httpGet(url) {
  if (typeof fetch !== 'function') throw new Error('searxng: global fetch unavailable (Node 18+ required)')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  const headers = { Accept: 'application/json' }
  if (REMOTE_TOKEN) headers.Authorization = `Bearer ${REMOTE_TOKEN}`
  let res
  try {
    res = await fetch(url, { method: 'GET', headers, signal: controller.signal })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : (e && e.message) || String(e)
    throw new Error(`GET ${hostOf(url)} failed: ${reason}`)
  }
  clearTimeout(timer)
  let raw = await res.text().catch(() => '')
  if (raw.length > MAX_RESPONSE_BYTES) raw = raw.slice(0, MAX_RESPONSE_BYTES)
  let parsed = null
  try { parsed = raw ? JSON.parse(raw) : null } catch { /* non-JSON (HTML error page) */ }
  if (!res.ok) {
    const m = parsed && (parsed.message || parsed.error) ? parsed.message || parsed.error : raw.slice(0, 300)
    throw new Error(`searxng HTTP ${res.status} from ${hostOf(url)}: ${typeof m === 'string' ? m : JSON.stringify(m)}`)
  }
  if (parsed === null) throw new Error(`searxng returned non-JSON from ${hostOf(url)} (is the json format enabled in settings.yml?)`)
  return parsed
}

const VALID_TIME = new Set(['day', 'week', 'month', 'year'])

async function searxngSearch(args) {
  const query = String(args?.query ?? '').trim()
  if (!query) return errResult('searxng_search', "a non-empty 'query' is required")
  if (!REMOTE_URL) {
    return errResult('searxng_search', 'searxng bridge not configured', {
      hint: 'set MATRIX_SEARXNG_URL to the self-hosted SearXNG instance',
    })
  }
  const max = Math.min(MAX_RESULTS_CAP, Math.max(1, Number.parseInt(args?.max_results ?? '', 10) || 8))
  const params = new URLSearchParams({ q: query, format: 'json', pageno: '1' })
  const categories = String(args?.categories ?? '').trim()
  if (categories) params.set('categories', categories)
  const language = String(args?.language ?? '').trim()
  if (language) params.set('language', language)
  const timeRange = String(args?.time_range ?? '').trim().toLowerCase()
  if (VALID_TIME.has(timeRange)) params.set('time_range', timeRange)

  const data = await httpGet(`${REMOTE_URL}/search?${params.toString()}`)
  const rows = Array.isArray(data?.results) ? data.results : []
  const results = rows.slice(0, max).map((r) => ({
    title: r.title || null,
    url: r.url || null,
    snippet: typeof r.content === 'string' ? r.content.slice(0, 1200) : null,
    engine: r.engine || (Array.isArray(r.engines) ? r.engines.join(',') : undefined),
    score: typeof r.score === 'number' ? r.score : undefined,
    category: r.category || undefined,
    published: r.publishedDate || undefined,
  }))
  return okResult({
    ok: true,
    tool: 'searxng_search',
    query,
    results,
    answers: Array.isArray(data?.answers) && data.answers.length ? data.answers : undefined,
    suggestions: Array.isArray(data?.suggestions) ? data.suggestions.slice(0, 8) : undefined,
    number_of_results: typeof data?.number_of_results === 'number' ? data.number_of_results : undefined,
  })
}

const impls = { searxng_search: searxngSearch }

// ── tool descriptors (advertised to the MCP client; MUST match the manifest) ──
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })

export const tools = [
  {
    name: 'searxng_search',
    description: 'Search the public web through a private SearXNG meta-search (aggregates Google/Bing/DuckDuckGo/etc.) and return ranked results (title, url, snippet, engine). No API key, no per-query cost. Read-only. Pair with the `fetch` or `firecrawl_scrape` tools to read a result URL in full. args: query (required), max_results? (1-50, default 8), categories? ("general"|"news"|"science"|"it"|"images"|"videos"|"map"), language? (e.g. "en"), time_range? ("day"|"week"|"month"|"year").',
    inputSchema: A({
      query: S('search query'),
      max_results: N('1-50, default 8'),
      categories: S('SearXNG category, e.g. "general" (default), "news", "science", "it"'),
      language: S('language hint, e.g. "en", "de"'),
      time_range: S('"day" | "week" | "month" | "year"'),
    }, ['query']),
  },
]

export const TOOL_NAMES = tools.map((t) => t.name)
const TOOL_SET = new Set(TOOL_NAMES)

// ── JSON-RPC stdio server (daemon-facing) ─────────────────────────────────────
const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? PROTOCOL_VERSION,
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    if (!TOOL_SET.has(name)) return errResult(name, `unknown tool: ${name}`)
    try {
      return await impls[name](args)
    } catch (err) {
      return errResult(name, err?.message ?? String(err))
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
// that ships a searxng server. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time. Offline (no network).
// MATRIX_SEARXNG_AGENTS_DIR overrides the manifest dir (used by tests).
function runSelftest() {
  console.log(`searxng: ${tools.length} tools (remote=${REMOTE_URL || 'UNSET'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.MATRIX_SEARXNG_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`searxng SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`searxng FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'searxng')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`searxng FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`searxng: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`searxng SELFTEST FAILED: no manifest under ${agentsDir} declares a searxng server`)
    process.exit(1)
  }
  if (drift) {
    console.error('searxng SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`searxng OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
