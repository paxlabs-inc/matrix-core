#!/usr/bin/env node
// tachyon — MCP stdio proxy bridging Matrix agents to the SHARED Fly tachyond
// (the agent-native Solidity/EVM engine) over its simple JSON-RPC transport.
//
// Why a proxy? Same shape as tools/browser/browser.mjs: the daemon spawns this
// over stdio (the transport executor/mcp/http.go supports), and it forwards
// tools/call to the remote tachyond at MATRIX_TACHYON_URL (POST /rpc, JSON-RPC
// 2.0 where the method IS the tool name and params ARE the request object).
// Unlike browser, tachyond is a plain request/response JSON endpoint — no SSE,
// no session id.
//
// Boot decoupling: initialize/tools/list are answered LOCALLY from the static
// registry baked alongside this file (tachyon-tools.json). The remote is
// contacted lazily on the first tools/call, so an unreachable engine never
// bricks daemon boot (executor/cmd/mcl-execute spawn failures are fatal) — the
// tachyon_* tools just return a structured error until the Fly app is reachable.
//
// Multi-tenant custody: the shared tachyond holds NO wallet seed. For WRITE
// tools (tachyon_deploy, and tachyon_call unless simulate_only) this proxy mints
// the caller's own embedded-wallet bearer from the daemon's ed25519 executor
// key (the SAME agent identity the paxeer bridge uses) and injects it as
// `wallet_token` so the engine signs + broadcasts as THIS agent. The seed never
// leaves the daemon; the shared box stays seedless.
//
// Remote endpoint: MATRIX_TACHYON_URL (e.g. http://matrix-tachyon.internal:8645/rpc).
// Optional auth:    MATRIX_TACHYON_TOKEN -> `Authorization: Bearer <tok>` (engine bearer).
// Optional:         MATRIX_TACHYON_TIMEOUT_MS (default 120000; compiles are slow).
// Agent identity (reused from the paxeer lane, no extra wiring in hosted mode):
//   PAXEER_AGENT_KEYFILE | MATRIX_EXECUTOR_KEYFILE | ${MATRIX_DATA_DIR|/data}/.matrix/executor.key
//   PAXEER_AGENT_LABEL   | MATRIX_USER_ID | MATRIX_DID_LABEL
//   PAXEER_WALLET_API    | PAXNET_WALLET_API (default https://connect.paxportwallet.com)
//   PAXEER_AGENT_AUTH_DISABLE=1 to disable token minting.
//
// Run `node tools/tachyon/tachyon.mjs --selftest` to smoke it offline (drift guard).

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { createPrivateKey, createPublicKey, sign as edSign } from 'node:crypto'

const SERVER_NAME = 'tachyon'
const SERVER_VERSION = '0.1.0'
const PROTOCOL_VERSION = '2024-11-05'

const REMOTE_URL = (process.env.MATRIX_TACHYON_URL || '').trim()
const REMOTE_TOKEN = (process.env.MATRIX_TACHYON_TOKEN || '').trim()
const TIMEOUT_MS = clampInt(process.env.MATRIX_TACHYON_TIMEOUT_MS, 120000, 2000, 600000)

// WRITE tools sign + broadcast and therefore need a forwarded wallet_token.
// tachyon_call is only a write when simulate_only is not true.
const WRITE_TOOLS = new Set(['tachyon_deploy', 'tachyon_call'])

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

// ── static tool registry (advertised verbatim) ───────────────────────────────
// executor/mcp Manager.verifyTools requires the advertised set to EQUAL the
// manifest set, so this file and agents/*.json must stay in bijection. Keep
// them in sync and re-run --selftest after any change.
const TOOLS_PATH = fileURLToPath(new URL('./tachyon-tools.json', import.meta.url))
let tools = []
try {
  tools = JSON.parse(readFileSync(TOOLS_PATH, 'utf8'))
} catch (err) {
  console.error(`tachyon: cannot load tool registry ${TOOLS_PATH}: ${err.message}`)
  process.exit(1)
}
const TOOL_NAMES = tools.map((t) => t.name)
const TOOL_SET = new Set(TOOL_NAMES)

// ── schema-driven arg re-coercion ─────────────────────────────────────────────
// The Matrix plan IR carries every tool-call arg as a string (MCL/ir/plan.go),
// and the executor's schema-blind coerceArg (executor/runtime/coerce.go) then
// greedily turns numeric-looking strings into JSON numbers — so chain_id "125"
// reaches this proxy as the number 125. tachyond's request structs type
// chain_id / value / spend_cap_wei / idempotency_key as Go strings, so a JSON
// number fails to unmarshal ("cannot unmarshal number into Go struct field ...
// of type string"). Using each tool's own inputSchema as the source of truth,
// re-stringify any value the schema declares a string; objects/arrays (sources,
// constructor_args, args, abi) and genuinely numeric fields (chain_register's
// chain_id) are left untouched.
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

// Wrap the engine envelope ({ok,data,error}) as an MCP CallToolResult, mirroring
// the native tachyon MCP server (pkg/mcp): the envelope is the text payload and
// ok=false marks the call as an error so the planner can branch on it.
function envelopeResult(envelope) {
  const isError = !!(envelope && envelope.ok === false)
  return {
    content: [{ type: 'text', text: JSON.stringify(envelope) }],
    isError,
  }
}

// ── JSON-RPC client to the remote tachyond (POST /rpc) ───────────────────────
function remoteHeaders() {
  const h = { 'Content-Type': 'application/json', Accept: 'application/json' }
  if (REMOTE_TOKEN) h.Authorization = `Bearer ${REMOTE_TOKEN}`
  return h
}

let rpcId = 1
async function rpc(method, params) {
  if (!REMOTE_URL) throw new Error('tachyon engine not configured')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  let res
  try {
    res = await fetch(REMOTE_URL, {
      method: 'POST',
      headers: remoteHeaders(),
      body: JSON.stringify({ jsonrpc: '2.0', id: rpcId++, method, params: params || {} }),
      signal: controller.signal,
    })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : (e && e.message) || String(e)
    throw new Error(`POST ${hostOf(REMOTE_URL)} failed: ${reason}`)
  }
  clearTimeout(timer)
  const raw = await safeText(res)
  if (!res.ok) throw new Error(`engine HTTP ${res.status}: ${raw.slice(0, 300)}`)
  let msg
  try {
    msg = raw ? JSON.parse(raw) : null
  } catch (e) {
    throw new Error(`engine returned non-JSON: ${raw.slice(0, 200)}`)
  }
  if (msg && msg.error) throw new Error(msg.error.message || JSON.stringify(msg.error))
  return msg ? msg.result : null
}

async function safeText(res) {
  try { return await res.text() } catch { return '' }
}
function hostOf(url) { try { return new URL(url).host } catch { return url } }

// ── agent-native DID auth (mints the per-agent embedded-wallet bearer) ───────
// Mirrors tools/paxeer/lib/agentauth.mjs and the Go daemon identity
// (executor/cmd/mcl-execute/identity.go): a 64-hex ed25519 SEED on disk;
// DID = did:matrix:<label>:<hex(pubkey)[:16]>. Challenge/verify against
// /v1/agent/auth/* yields a short-lived token the engine forwards to the wallet.
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')

const AGENT = {
  keyfile:
    pickEnv('PAXEER_AGENT_KEYFILE', 'MATRIX_EXECUTOR_KEYFILE') ||
    `${pickEnv('MATRIX_DATA_DIR') || '/data'}/.matrix/executor.key`,
  label: pickEnv('PAXEER_AGENT_LABEL', 'MATRIX_USER_ID', 'MATRIX_DID_LABEL') || 'executor',
  walletBase: (pickEnv('PAXEER_WALLET_API', 'PAXNET_WALLET_API') || 'https://connect.paxportwallet.com').replace(/\/+$/, '').replace(/\/v1$/, ''),
  disabled: pickEnv('PAXEER_AGENT_AUTH_DISABLE') === '1',
}

function pickEnv(...names) {
  for (const n of names) {
    const v = process.env[n]
    if (v != null && String(v).trim() !== '') return String(v).trim()
  }
  return undefined
}

let _identity = null
let _walletToken = null

function loadIdentity() {
  if (_identity) return _identity
  const raw = readFileSync(AGENT.keyfile, 'utf8').trim()
  if (!/^[0-9a-fA-F]{64}$/.test(raw)) {
    throw new Error(`tachyon agent auth: ${AGENT.keyfile} is not a 64-hex ed25519 seed`)
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

async function walletJson(method, path, body, token) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), 30000)
  let res
  try {
    const headers = { 'Content-Type': 'application/json', Accept: 'application/json' }
    if (token) headers.Authorization = `Bearer ${token}`
    res = await fetch(`${AGENT.walletBase}${path}`, {
      method,
      headers,
      body: body != null ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? 'timed out after 30000ms' : (e && e.message) || String(e)
    throw new Error(`wallet ${path} failed: ${reason}`)
  }
  clearTimeout(timer)
  const raw = await safeText(res)
  if (!res.ok) {
    const err = new Error(`wallet ${path} http ${res.status}: ${raw.slice(0, 200)}`)
    err.status = res.status
    throw err
  }
  return raw ? JSON.parse(raw) : null
}

async function mintWalletToken(force = false) {
  if (_walletToken && !force) return _walletToken
  const id = loadIdentity()
  const ch = await walletJson('POST', '/v1/agent/auth/challenge', { did: id.did })
  if (!ch || !ch.message || !ch.nonce) {
    throw new Error('tachyon agent auth: challenge returned no message/nonce')
  }
  const signature = edSign(null, Buffer.from(ch.message, 'utf8'), id.privateKey).toString('hex')
  const vr = await walletJson('POST', '/v1/agent/auth/verify', {
    did: id.did,
    public_key: id.pubHex,
    nonce: ch.nonce,
    signature,
  })
  if (!vr || !vr.token) throw new Error('tachyon agent auth: verify returned no token')
  _walletToken = vr.token
  return _walletToken
}

// needsWalletToken reports whether a call will broadcast (and therefore must
// carry a forwarded agent bearer for the seedless shared engine to sign).
function needsWalletToken(name, args) {
  if (!WRITE_TOOLS.has(name)) return false
  if (name === 'tachyon_call' && args && args.simulate_only === true) return false
  return true
}

// ── JSON-RPC stdio server (daemon-facing) ─────────────────────────────────────
async function callRemoteTool(name, args) {
  const params = coerceArgsToSchema(name, { ...(args || {}) })
  if (needsWalletToken(name, params) && !params.wallet_token) {
    if (AGENT.disabled) {
      return errResult(name, 'broadcast requires an agent wallet token, but agent auth is disabled (PAXEER_AGENT_AUTH_DISABLE=1)')
    }
    if (!isAgentConfigured()) {
      return errResult(name, 'broadcast requires an agent wallet token, but no ed25519 executor key is available', {
        hint: `expected a 64-hex seed at ${AGENT.keyfile}`,
      })
    }
    try {
      params.wallet_token = await mintWalletToken()
    } catch (e) {
      return errResult(name, `agent wallet auth failed: ${e?.message ?? String(e)}`)
    }
  }
  const envelope = await rpc(name, params)
  return envelopeResult(envelope)
}

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
      return errResult(name, 'tachyon engine not configured', {
        hint: 'set MATRIX_TACHYON_URL to the shared Fly tachyond (e.g. http://matrix-tachyon.internal:8645/rpc)',
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

// `--selftest`: list the registry, then verify it against every agent manifest
// that ships a tachyon server. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time. Offline (no network).
// MATRIX_TACHYON_AGENTS_DIR overrides the manifest dir (used by tests).
function runSelftest() {
  console.log(`tachyon: ${tools.length} tools (remote=${REMOTE_URL || 'UNSET'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.MATRIX_TACHYON_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`tachyon SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`tachyon FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'tachyon')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`tachyon FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`tachyon: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`tachyon SELFTEST FAILED: no manifest under ${agentsDir} declares a tachyon server`)
    process.exit(1)
  }
  if (drift) {
    console.error('tachyon SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }

  // arg-coercion regression: the executor delivers numeric-looking string args
  // as JSON numbers; schema string fields MUST be re-stringified for tachyond,
  // while containers and numeric fields stay intact.
  const cFails = []
  const dep = coerceArgsToSchema('tachyon_deploy', {
    chain_id: 125,
    idempotency_key: 1,
    contract: 'MatrixFlowTest',
    constructor_args: ['MatrixFlowTest', 1000000],
  })
  if (dep.chain_id !== '125') cFails.push(`deploy.chain_id ${JSON.stringify(dep.chain_id)} != "125"`)
  if (dep.idempotency_key !== '1') cFails.push(`deploy.idempotency_key ${JSON.stringify(dep.idempotency_key)} != "1"`)
  if (dep.contract !== 'MatrixFlowTest') cFails.push('deploy.contract mutated')
  if (!Array.isArray(dep.constructor_args) || dep.constructor_args[1] !== 1000000) {
    cFails.push('deploy.constructor_args array mangled')
  }
  const reg = coerceArgsToSchema('tachyon_chain_register', { chain_id: 125 })
  if (reg.chain_id !== 125) cFails.push(`chain_register.chain_id ${JSON.stringify(reg.chain_id)} should stay numeric`)
  const lk = coerceArgsToSchema('tachyon_registry_lookup', { chain_id: 125, idempotency_key: 'k' })
  if (lk.chain_id !== '125') cFails.push(`registry_lookup.chain_id ${JSON.stringify(lk.chain_id)} != "125"`)
  if (cFails.length) {
    console.error('tachyon SELFTEST FAILED (arg coercion): ' + cFails.join('; '))
    process.exit(1)
  }
  console.log('tachyon: arg coercion OK (string fields re-stringified, containers + numeric fields intact)')

  console.log(`tachyon OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
