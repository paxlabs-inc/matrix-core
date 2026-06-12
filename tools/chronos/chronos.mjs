#!/usr/bin/env node
// chronos — MCP stdio proxy bridging Matrix agents to the SHARED chronosd
// (the centralized agent scheduler / wake control plane) over its REST API.
//
// Same shape as tools/tachyon/tachyon.mjs + tools/uwac/uwac.mjs: the daemon
// spawns this over stdio (the transport executor/mcp/http.go supports), answers
// initialize/tools/list LOCALLY from the static registry baked alongside this
// file (chronos-tools.json), and forwards tools/call to the remote chronosd at
// MATRIX_CHRONOS_URL. Boot is decoupled: an unreachable scheduler never bricks
// daemon boot (executor spawn failures are fatal) — the alarm_* tools just
// return a structured error until chronosd is reachable.
//
// Two-layer auth (chronos.frozen.kvx [auth]):
//   1. transport: every request carries MATRIX_CHRONOS_TOKEN as a Bearer
//      (proves "a legitimate Matrix daemon").
//   2. principal: the alarm_* tools additionally carry an agent-DID principal
//      token in X-Chronos-Agent. The proxy mints it by ed25519-signing
//      chronosd's challenge with the daemon's executor key (the SAME identity
//      the paxeer/tachyon/uwac lanes use) — so chronosd scopes every alarm to
//      THIS owner (did:matrix:<MATRIX_USER_ID>:<keyfp>) and the wake target
//      resolves from the DID alone.
//
// Remote endpoint: MATRIX_CHRONOS_URL (e.g. http://matrix-chronos.internal:9096).
// Optional:        MATRIX_CHRONOS_TOKEN -> transport `Authorization: Bearer`.
// Optional:        MATRIX_CHRONOS_TIMEOUT_MS (default 30000).
// Agent identity (reused from the shared agent lane):
//   PAXEER_AGENT_KEYFILE | MATRIX_EXECUTOR_KEYFILE | ${MATRIX_DATA_DIR|/data}/.matrix/executor.key
//   MATRIX_USER_ID | PAXEER_AGENT_LABEL | MATRIX_DID_LABEL  (the DID label = owner)
//   MATRIX_CHRONOS_AUTH_DISABLE=1 to disable principal-token minting.
//
// Run `node tools/chronos/chronos.mjs --selftest` to smoke it offline (drift guard).

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { createPrivateKey, createPublicKey, sign as edSign } from 'node:crypto'

const SERVER_NAME = 'chronos'
const SERVER_VERSION = '0.1.0'
const PROTOCOL_VERSION = '2024-11-05'

const REMOTE_URL = (process.env.MATRIX_CHRONOS_URL || '').trim().replace(/\/+$/, '')
const TRANSPORT_TOKEN = (process.env.MATRIX_CHRONOS_TOKEN || '').trim()
const TIMEOUT_MS = clampInt(process.env.MATRIX_CHRONOS_TIMEOUT_MS, 30000, 2000, 300000)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

// ── static tool registry (advertised verbatim) ───────────────────────────────
// executor/mcp Manager.verifyTools requires the advertised set to EQUAL the
// manifest set, so this file and agents/*.json must stay in bijection.
const TOOLS_PATH = fileURLToPath(new URL('./chronos-tools.json', import.meta.url))
let tools = []
try {
  tools = JSON.parse(readFileSync(TOOLS_PATH, 'utf8'))
} catch (err) {
  console.error(`chronos: cannot load tool registry ${TOOLS_PATH}: ${err.message}`)
  process.exit(1)
}
const TOOL_NAMES = tools.map((t) => t.name)
const TOOL_SET = new Set(TOOL_NAMES)

// ── schema-driven arg re-coercion ─────────────────────────────────────────────
// The Matrix plan IR carries every tool-call arg as a string (MCL/ir/plan.go),
// and the executor's schema-blind coerceArg greedily turns numeric-looking
// strings into JSON numbers. chronosd types idempotency_key / fire_at /
// cron_expr / label etc. as Go strings, so a JSON number fails to unmarshal.
// Using each tool's own inputSchema as the source of truth, re-stringify any
// value the schema declares a string; objects (payload) and genuinely numeric
// fields (delay_seconds, max_failures, limit) are left untouched.
const STRING_PROPS_BY_TOOL = new Map(
  tools.map((t) => {
    const props = t.inputSchema?.properties || {}
    const stringKeys = new Set()
    for (const [key, spec] of Object.entries(props)) {
      const declared = spec && spec.type
      if (declared === 'string' || (Array.isArray(declared) && declared.includes('string'))) {
        stringKeys.add(key)
      }
    }
    return [t.name, stringKeys]
  })
)

function coerceArgsToSchema(name, args) {
  const stringKeys = STRING_PROPS_BY_TOOL.get(name)
  if (!stringKeys || !args || typeof args !== 'object') return args
  for (const key of stringKeys) {
    const val = args[key]
    if (typeof val === 'number' || typeof val === 'boolean') {
      args[key] = String(val)
    }
  }
  return args
}

// ── result shaping ───────────────────────────────────────────────────────────
function errResult(tool, error, extra = {}) {
  return {
    content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }],
    isError: true,
  }
}

// Wrap the chronosd envelope ({ok,data,error}) as an MCP CallToolResult.
function envelopeResult(envelope) {
  const isError = !!(envelope && envelope.ok === false)
  return {
    content: [{ type: 'text', text: JSON.stringify(envelope) }],
    isError,
  }
}

// ── HTTP client to the remote chronosd ───────────────────────────────────────
function transportHeaders(extra = {}) {
  const h = { 'Content-Type': 'application/json', Accept: 'application/json', ...extra }
  if (TRANSPORT_TOKEN) h.Authorization = `Bearer ${TRANSPORT_TOKEN}`
  return h
}

async function httpJson(method, path, { body, headers, token } = {}) {
  if (!REMOTE_URL) throw new Error('chronos scheduler not configured')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  const hdrs = transportHeaders(headers)
  if (token) hdrs['X-Chronos-Agent'] = token
  let res
  try {
    res = await fetch(`${REMOTE_URL}${path}`, {
      method,
      headers: hdrs,
      body: body != null ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : (e && e.message) || String(e)
    throw httpError(`POST ${hostOf(REMOTE_URL)} failed: ${reason}`, 0)
  }
  clearTimeout(timer)
  const raw = await safeText(res)
  let msg = null
  try {
    msg = raw ? JSON.parse(raw) : null
  } catch {
    throw httpError(`chronosd returned non-JSON (${res.status}): ${raw.slice(0, 200)}`, res.status)
  }
  if (!res.ok) {
    const detail = msg && msg.error ? msg.error.message || msg.error.code : raw.slice(0, 200)
    throw httpError(`chronosd HTTP ${res.status}: ${detail}`, res.status)
  }
  return msg
}

function httpError(message, status) {
  const e = new Error(message)
  e.status = status
  return e
}

async function safeText(res) {
  try { return await res.text() } catch { return '' }
}
function hostOf(url) { try { return new URL(url).host } catch { return url } }

// ── agent-native DID auth (mints the principal token against chronosd) ───────
// Mirrors tools/paxeer/lib/agentauth.mjs + the Go daemon identity
// (executor/cmd/mcl-execute/identity.go): a 64-hex ed25519 SEED on disk;
// DID = did:matrix:<label>:<hex(pubkey)[:16]>. chronosd is BOTH challenger and
// verifier (internal/auth), so we sign the exact `message` it returns.
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')

const AGENT = {
  keyfile:
    pickEnv('PAXEER_AGENT_KEYFILE', 'MATRIX_EXECUTOR_KEYFILE') ||
    `${pickEnv('MATRIX_DATA_DIR') || '/data'}/.matrix/executor.key`,
  // The DID label IS the owner: chronosd derives the wake target from it, so
  // prefer MATRIX_USER_ID (the Supabase user uuid the router routes on).
  label: pickEnv('MATRIX_USER_ID', 'PAXEER_AGENT_LABEL', 'MATRIX_DID_LABEL') || 'executor',
  disabled: pickEnv('MATRIX_CHRONOS_AUTH_DISABLE') === '1',
}

function pickEnv(...names) {
  for (const n of names) {
    const v = process.env[n]
    if (v != null && String(v).trim() !== '') return String(v).trim()
  }
  return undefined
}

let _identity = null
let _agentToken = null

function loadIdentity() {
  if (_identity) return _identity
  const raw = readFileSync(AGENT.keyfile, 'utf8').trim()
  if (!/^[0-9a-fA-F]{64}$/.test(raw)) {
    throw new Error(`chronos agent auth: ${AGENT.keyfile} is not a 64-hex ed25519 seed`)
  }
  const seed = Buffer.from(raw, 'hex')
  const privateKey = createPrivateKey({
    key: Buffer.concat([ED25519_PKCS8_PREFIX, seed]),
    format: 'der',
    type: 'pkcs8',
  })
  const spki = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
  const pubHex = Buffer.from(spki.subarray(spki.length - 32)).toString('hex')
  _identity = { did: `did:matrix:${AGENT.label}:${pubHex.slice(0, 16)}`, pubHex, privateKey }
  return _identity
}

function isAgentConfigured() {
  if (AGENT.disabled) return false
  try {
    loadIdentity()
    return true
  } catch {
    return false
  }
}

async function mintAgentToken(force = false) {
  if (_agentToken && !force) return _agentToken
  const id = loadIdentity()
  const ch = await httpJson('POST', '/v1/agent/auth/challenge', { body: { did: id.did } })
  const data = ch && ch.data
  if (!data || !data.message || !data.nonce) {
    throw new Error('chronos agent auth: challenge returned no message/nonce')
  }
  const signature = edSign(null, Buffer.from(data.message, 'utf8'), id.privateKey).toString('hex')
  const vr = await httpJson('POST', '/v1/agent/auth/verify', {
    body: { did: id.did, public_key: id.pubHex, nonce: data.nonce, signature },
  })
  const token = vr && vr.data && vr.data.token
  if (!token) throw new Error('chronos agent auth: verify returned no token')
  _agentToken = token
  return _agentToken
}

// Authed call against the alarm lane; re-mints once on a 401.
async function alarmCall(method, path, body, retry = true) {
  if (AGENT.disabled) {
    throw new Error('chronos principal auth is disabled (MATRIX_CHRONOS_AUTH_DISABLE=1)')
  }
  if (!isAgentConfigured()) {
    throw new Error(`no ed25519 executor key available (expected a 64-hex seed at ${AGENT.keyfile})`)
  }
  const token = await mintAgentToken()
  try {
    return await httpJson(method, path, { body, token })
  } catch (e) {
    if (e.status === 401 && retry) {
      await mintAgentToken(true)
      return alarmCall(method, path, body, false)
    }
    throw e
  }
}

// ── tool dispatch (maps MCP tool calls onto the chronosd REST surface) ───────
async function callRemoteTool(name, rawArgs) {
  const args = coerceArgsToSchema(name, { ...(rawArgs || {}) })
  switch (name) {
    case 'alarm_set':
      return envelopeResult(await alarmCall('POST', '/v1/alarms', args))
    case 'alarm_list': {
      const q = args.limit != null ? `?limit=${encodeURIComponent(args.limit)}` : ''
      return envelopeResult(await alarmCall('GET', `/v1/alarms${q}`, null))
    }
    case 'alarm_get': {
      if (!args.id) return errResult(name, 'id is required')
      return envelopeResult(await alarmCall('GET', `/v1/alarms/${encodeURIComponent(args.id)}`, null))
    }
    case 'alarm_cancel': {
      if (!args.id) return errResult(name, 'id is required')
      return envelopeResult(await alarmCall('DELETE', `/v1/alarms/${encodeURIComponent(args.id)}`, null))
    }
    default:
      return errResult(name, `unknown tool: ${name}`)
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
      return errResult(name, 'chronos scheduler not configured', {
        hint: 'set MATRIX_CHRONOS_URL to the shared chronosd (e.g. http://matrix-chronos.internal:9096)',
      })
    }
    try {
      return await callRemoteTool(name, args)
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

// `--selftest`: list the registry, verify bijection against every agent
// manifest that ships a chronos server, then exercise arg coercion. Offline.
function runSelftest() {
  console.log(`chronos: ${tools.length} tools (remote=${REMOTE_URL || 'UNSET'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.MATRIX_CHRONOS_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`chronos SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`chronos FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'chronos')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`chronos FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits: ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits: ${manifestOnly.join(', ')}`)
    } else {
      console.log(`chronos: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`chronos SELFTEST FAILED: no manifest under ${agentsDir} declares a chronos server`)
    process.exit(1)
  }
  if (drift) {
    console.error('chronos SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }

  // arg-coercion regression: string fields re-stringified, numeric + object intact.
  const cFails = []
  const set = coerceArgsToSchema('alarm_set', {
    kind: 'once',
    delay_seconds: 600,
    max_failures: 3,
    idempotency_key: 12345,
    wake_message: 'check the thing',
    payload: { n: 1 },
  })
  if (set.idempotency_key !== '12345') cFails.push(`alarm_set.idempotency_key ${JSON.stringify(set.idempotency_key)} != "12345"`)
  if (set.delay_seconds !== 600) cFails.push('alarm_set.delay_seconds should stay numeric')
  if (set.max_failures !== 3) cFails.push('alarm_set.max_failures should stay numeric')
  if (typeof set.payload !== 'object') cFails.push('alarm_set.payload object mangled')
  const get = coerceArgsToSchema('alarm_get', { id: 42 })
  if (get.id !== '42') cFails.push(`alarm_get.id ${JSON.stringify(get.id)} != "42"`)
  if (cFails.length) {
    console.error('chronos SELFTEST FAILED (arg coercion): ' + cFails.join('; '))
    process.exit(1)
  }
  console.log('chronos: arg coercion OK (string fields re-stringified, numeric + object intact)')

  console.log(`chronos OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
