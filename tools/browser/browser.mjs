#!/usr/bin/env node
// browser — MCP stdio proxy bridging Matrix agents to the SHARED Fly
// Playwright browser (@playwright/mcp) over its Streamable-HTTP transport.
//
// Why a proxy? The daemon's own MCP HTTP transport (executor/mcp/http.go)
// is a simplified Streamable HTTP: synchronous plain-JSON only, no SSE, no
// Mcp-Session-Id (SSE deferred to v1.1). @playwright/mcp --port speaks the
// FULL Streamable HTTP (SSE responses + session handshake), so the daemon
// cannot talk to it directly. This bridge runs locally on stdio (which the
// daemon transport DOES support) and forwards to the remote browser using a
// proper Streamable-HTTP client (session-id + SSE parsing) implemented below.
//
// Boot decoupling: initialize/tools/list are answered LOCALLY from a static
// registry baked alongside this file (playwright-tools.json, the exact
// tools/list of the pinned @playwright/mcp). The remote is contacted lazily
// on the first tools/call, so an unreachable browser never bricks daemon boot
// (executor/cmd/mcl-execute spawn failures are fatal) — the browser_* tools
// just return a structured error until the Fly app is reachable.
//
// Isolation: each daemon's proxy opens its OWN remote session, so the Fly
// server running with --isolated hands each tenant a fresh browser context.
//
// Remote endpoint: MATRIX_BROWSER_URL (e.g. http://matrix-browser.flycast:8931/mcp).
// Optional auth: MATRIX_BROWSER_TOKEN -> sent as `Authorization: Bearer <tok>`.
// Optional: MATRIX_BROWSER_TIMEOUT_MS (default 60000).
//
// Wire protocol (daemon<->proxy) mirrors tools/paxeer/paxeer-net.mjs.
// Run `node tools/browser/browser.mjs --selftest` to smoke it offline.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const SERVER_NAME = 'browser'
const SERVER_VERSION = '0.1.0'
const PROTOCOL_VERSION = '2024-11-05'

const REMOTE_URL = (process.env.MATRIX_BROWSER_URL || '').trim()
const REMOTE_TOKEN = (process.env.MATRIX_BROWSER_TOKEN || '').trim()
const TIMEOUT_MS = clampInt(process.env.MATRIX_BROWSER_TIMEOUT_MS, 60000, 2000, 300000)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

// ── static tool registry (the pinned @playwright/mcp tools/list) ──────────────
// Loaded verbatim so the proxy advertises EXACTLY what the remote serves
// (executor/mcp Manager.verifyTools requires the manifest tool set to equal
// the advertised set). Regenerate playwright-tools.json from the pinned
// server when bumping @playwright/mcp, and re-run --selftest.
const TOOLS_PATH = fileURLToPath(new URL('./playwright-tools.json', import.meta.url))
let tools = []
try {
  tools = JSON.parse(readFileSync(TOOLS_PATH, 'utf8'))
} catch (err) {
  console.error(`browser: cannot load tool registry ${TOOLS_PATH}: ${err.message}`)
  process.exit(1)
}
const TOOL_NAMES = tools.map((t) => t.name)
const TOOL_SET = new Set(TOOL_NAMES)

// ── result shaping ───────────────────────────────────────────────────────────
function errResult(tool, error, extra = {}) {
  return {
    content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }],
    isError: true,
  }
}

// ── Streamable-HTTP client to the remote @playwright/mcp ──────────────────────
// Minimal but spec-correct for request/response: POST one JSON-RPC frame,
// accept either application/json or a text/event-stream containing the reply,
// and carry the Mcp-Session-Id returned by initialize on every later request.
let sessionId = null
let initialized = false
let initInFlight = null

function remoteHeaders() {
  const h = { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' }
  if (REMOTE_TOKEN) h.Authorization = `Bearer ${REMOTE_TOKEN}`
  if (sessionId) h['Mcp-Session-Id'] = sessionId
  return h
}

// Extract the JSON-RPC message from a response that may be plain JSON or an
// SSE stream ("event: message\ndata: {json}\n\n", possibly multiple events).
function parseFrame(contentType, raw) {
  if (contentType && contentType.includes('text/event-stream')) {
    let last = null
    for (const block of raw.split(/\n\n/)) {
      const dataLines = block.split(/\r?\n/).filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trim())
      if (!dataLines.length) continue
      try {
        const msg = JSON.parse(dataLines.join('\n'))
        if (msg && (msg.result !== undefined || msg.error !== undefined)) last = msg
        else if (last === null) last = msg
      } catch { /* skip non-JSON event */ }
    }
    return last
  }
  return raw ? JSON.parse(raw) : null
}

async function rpc(method, params, id) {
  if (!REMOTE_URL) throw new Error('browser bridge not configured')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  let res
  try {
    res = await fetch(REMOTE_URL, {
      method: 'POST',
      headers: remoteHeaders(),
      body: JSON.stringify(id === undefined ? { jsonrpc: '2.0', method, params } : { jsonrpc: '2.0', id, method, params }),
      signal: controller.signal,
    })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : (e && e.message) || String(e)
    throw new Error(`POST ${hostOf(REMOTE_URL)} failed: ${reason}`)
  }
  clearTimeout(timer)
  const sid = res.headers.get('mcp-session-id')
  if (sid) sessionId = sid
  if (res.status === 404 || res.status === 400) {
    // Session expired/unknown — signal caller to re-handshake.
    initialized = false
    sessionId = null
    const body = await safeText(res)
    const e = new Error(`remote ${res.status}: ${body.slice(0, 200)}`)
    e.sessionLost = true
    throw e
  }
  const raw = await safeText(res)
  if (!res.ok) throw new Error(`remote HTTP ${res.status}: ${raw.slice(0, 200)}`)
  if (id === undefined) return null // notification
  const msg = parseFrame(res.headers.get('content-type') || '', raw)
  if (msg && msg.error) throw new Error(msg.error.message || JSON.stringify(msg.error))
  return msg ? msg.result : null
}

async function safeText(res) {
  try { return await res.text() } catch { return '' }
}
function hostOf(url) { try { return new URL(url).host } catch { return url } }

let rpcId = 1
async function ensureRemote() {
  if (initialized) return
  if (initInFlight) return initInFlight
  initInFlight = (async () => {
    sessionId = null
    await rpc('initialize', {
      protocolVersion: PROTOCOL_VERSION,
      capabilities: {},
      clientInfo: { name: 'matrix-browser-proxy', version: SERVER_VERSION },
    }, rpcId++)
    await rpc('notifications/initialized', {}) // notification (no id)
    initialized = true
  })()
  try {
    await initInFlight
  } finally {
    initInFlight = null
  }
}

async function callRemoteTool(name, args) {
  await ensureRemote()
  try {
    return await rpc('tools/call', { name, arguments: args || {} }, rpcId++)
  } catch (e) {
    if (e && e.sessionLost) {
      // One transparent re-handshake on session loss.
      await ensureRemote()
      return await rpc('tools/call', { name, arguments: args || {} }, rpcId++)
    }
    throw e
  }
}

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
    if (!TOOL_SET.has(name)) {
      return errResult(name, `unknown tool: ${name}`)
    }
    if (!REMOTE_URL) {
      return errResult(name, 'browser bridge not configured', {
        hint: 'set MATRIX_BROWSER_URL to the shared Fly Playwright server (e.g. http://matrix-browser.internal:8931/mcp)',
      })
    }
    try {
      const result = await callRemoteTool(name, args)
      // Remote returns a CallToolResult ({content,isError}); pass it through.
      return result ?? errResult(name, 'empty result from remote browser')
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
// that ships a browser server. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time. Offline (no network).
// MATRIX_BROWSER_AGENTS_DIR overrides the manifest dir (used by tests).
function runSelftest() {
  console.log(`browser: ${tools.length} tools (remote=${REMOTE_URL || 'UNSET'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.MATRIX_BROWSER_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`browser SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`browser FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'browser')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`browser FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`browser: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`browser SELFTEST FAILED: no manifest under ${agentsDir} declares a browser server`)
    process.exit(1)
  }
  if (drift) {
    console.error('browser SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`browser OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
