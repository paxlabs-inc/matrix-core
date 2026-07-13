#!/usr/bin/env node
// deus — MCP stdio proxy bridging Matrix agents to the Deus gateway HTTP API.
// Mirrors tools/browser/browser.mjs: local tools/list, lazy remote on tools/call.

import { createInterface } from 'node:readline'
import { mkdirSync, readdirSync, readFileSync, realpathSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { createPrivateKey, createPublicKey, sign as edSign } from 'node:crypto'

const SERVER_NAME = 'deus'
const SERVER_VERSION = '0.1.0-phase2'
const PROTOCOL_VERSION = '2024-11-05'

const BASE_URL = (process.env.MATRIX_DEUS_URL || 'https://deus.paxeer.app').replace(/\/+$/, '')
const TIMEOUT_MS = clampInt(process.env.MATRIX_DEUS_TIMEOUT_MS, 60000, 2000, 300000)
const WRITE_TOOLS = new Set(['deus_invoke'])

const TOOLS_PATH = fileURLToPath(new URL('./deus-tools.json', import.meta.url))
const tools = JSON.parse(readFileSync(TOOLS_PATH, 'utf8'))
const TOOL_NAMES = tools.map((t) => t.name)
const TOOL_SET = new Set(TOOL_NAMES)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

function errResult(tool, error, extra = {}) {
  return {
    content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }],
    isError: true,
  }
}

function okResult(data) {
  return { content: [{ type: 'text', text: JSON.stringify({ ok: true, data }) }] }
}

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
  if (!/^[0-9a-fA-F]{64}$/.test(raw)) throw new Error(`deus agent auth: ${AGENT.keyfile} is not a 64-hex ed25519 seed`)
  const seed = Buffer.from(raw, 'hex')
  const privateKey = createPrivateKey({ key: Buffer.concat([ED25519_PKCS8_PREFIX, seed]), format: 'der', type: 'pkcs8' })
  const spki = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
  const pubHex = Buffer.from(spki.subarray(spki.length - 32)).toString('hex')
  _identity = { did: `did:matrix:${AGENT.label}:${pubHex.slice(0, 16)}`, pubHex, privateKey }
  return _identity
}

async function mintWalletToken() {
  if (_walletToken) return _walletToken
  if (AGENT.disabled) return null
  const id = loadIdentity()
  const chRes = await fetch(`${AGENT.walletBase}/v1/agent/auth/challenge`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ did: id.did }),
  })
  const ch = await chRes.json()
  const signature = edSign(null, Buffer.from(ch.message, 'utf8'), id.privateKey).toString('hex')
  const vrRes = await fetch(`${AGENT.walletBase}/v1/agent/auth/verify`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ did: id.did, public_key: id.pubHex, nonce: ch.nonce, signature }),
  })
  const vr = await vrRes.json()
  _walletToken = vr.token
  return _walletToken
}

async function rawFetch(method, path, body, headers = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  try {
    const res = await fetch(`${BASE_URL}${path}`, {
      method,
      headers: { Accept: 'application/json', 'Content-Type': 'application/json', ...headers },
      body: body != null ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
    clearTimeout(timer)
    const raw = await res.text()
    let data
    try { data = raw ? JSON.parse(raw) : null } catch { data = { raw } }
    return { status: res.status, data, headers: res.headers }
  } catch (e) {
    clearTimeout(timer)
    throw e
  }
}

async function deusFetch(method, path, body, bearer) {
  const headers = {}
  if (bearer) headers.Authorization = `Bearer ${bearer}`
  try {
    headers['X-Caller-DID'] = loadIdentity().did
  } catch {}
  const res = await rawFetch(method, path, body, headers)
  if (res.status < 200 || res.status >= 300) {
    const err = new Error(res.data?.message || res.data?.error || `HTTP ${res.status}`)
    err.status = res.status
    err.data = res.data
    throw err
  }
  return res.data
}

// ── LXP: HTTP-native LayerX payments under the owner leash ──────────────────
// A priced invoke answers 402 with lxp/1 terms (a prefetched LayerX nonce +
// USDX amount). Within the leash this bridge signs the canonical LayerX
// intent with the executor key and retries once; over leash it surfaces the
// terms to the agent and never signs. The signing preimage is byte-identical
// to layerxd's auth.IntentMessage (lockstep proven by cross-implementation
// vectors against deus/pkg/lxp).

export function parseUSDXMicro(s) {
  if (s == null || String(s).trim() === '') return null
  const m = /^([0-9]+)(?:\.([0-9]{1,6}))?$/.exec(String(s).trim())
  if (!m) return null
  return Number(m[1]) * 1_000_000 + Number((m[2] || '').padEnd(6, '0'))
}

export function formatUSDX(micro) {
  return `${Math.floor(micro / 1_000_000)}.${String(micro % 1_000_000).padStart(6, '0')}`
}

export function intentMessage(op, did, nonce, ...fields) {
  return `matrix-layerx-intent:${op}:${did}:${fields.join(':')}:${nonce}`
}

export function payPreimage(p) {
  const fields = [p.to_did, p.amount_usdx]
  if (p.ref) fields.push(p.ref)
  return intentMessage('pay', p.from_did, p.nonce, ...fields)
}

export function holdPreimage(p, ttlSeconds, captorDid) {
  return intentMessage('hold', p.from_did, p.nonce, p.to_did, p.amount_usdx, String(ttlSeconds), p.ref || '', captorDid)
}

export function signPayment(terms, identity) {
  const p = {
    from_did: identity.did,
    public_key: identity.pubHex,
    nonce: terms.nonce,
    to_did: terms.pay_to,
    amount_usdx: terms.amount_usdx,
    mode: terms.mode || 'exact',
  }
  if (terms.ref) p.ref = terms.ref
  const preimage = p.mode === 'hold'
    ? holdPreimage(p, terms.ttl_s || 120, terms.captor_did || '')
    : payPreimage(p)
  p.signature = edSign(null, Buffer.from(preimage, 'utf8'), identity.privateKey).toString('hex')
  return p
}

export function encodePaymentHeader(p) {
  return Buffer.from(JSON.stringify(p)).toString('base64url')
}

export function decodeReceiptHeader(h) {
  if (!h) return null
  try { return JSON.parse(Buffer.from(h, 'base64url').toString('utf8')) } catch { return null }
}

const LEASH = {
  maxSpendMicro: parseUSDXMicro(pickEnv('LAYERX_MAX_SPEND_USDX')),
  maxDailyMicro: parseUSDXMicro(pickEnv('LAYERX_MAX_DAILY_USDX')),
  journalPath:
    pickEnv('LAYERX_SPEND_JOURNAL') ||
    join(pickEnv('MATRIX_DATA_DIR') || '/data', '.matrix', 'lxp-spend.json'),
}

const DAILY_WINDOW_MS = 24 * 3600 * 1000

export function readSpendJournal(path, now = Date.now()) {
  let entries = []
  try {
    entries = JSON.parse(readFileSync(path, 'utf8')).entries || []
  } catch {}
  return entries.filter((e) => Number.isFinite(e?.ts) && Number.isFinite(e?.micro) && now - e.ts < DAILY_WINDOW_MS)
}

export function recordSpend(micro, path, now = Date.now()) {
  const entries = readSpendJournal(path, now)
  entries.push({ ts: now, micro })
  try {
    mkdirSync(dirname(path), { recursive: true })
    writeFileSync(path, JSON.stringify({ entries }))
  } catch {}
}

// leashCheck decides whether the bridge may sign a charge of amountMicro.
// No per-call leash configured means NO invisible payments — the owner opts
// into auto-payment by setting LAYERX_MAX_SPEND_USDX.
export function leashCheck(amountMicro, leash = LEASH, now = Date.now()) {
  if (amountMicro == null) return { ok: false, reason: 'invalid_terms', detail: 'challenge carried no parseable amount_usdx' }
  if (leash.maxSpendMicro == null) {
    return {
      ok: false,
      reason: 'auto_payment_disabled',
      detail: 'no spend leash configured; set LAYERX_MAX_SPEND_USDX (per call) and optionally LAYERX_MAX_DAILY_USDX to let deus pay lxp challenges automatically',
    }
  }
  if (amountMicro > leash.maxSpendMicro) {
    return {
      ok: false,
      reason: 'over_per_call_leash',
      detail: `charge ${formatUSDX(amountMicro)} USDX exceeds LAYERX_MAX_SPEND_USDX ${formatUSDX(leash.maxSpendMicro)}`,
    }
  }
  if (leash.maxDailyMicro != null) {
    const spent = readSpendJournal(leash.journalPath, now).reduce((a, e) => a + e.micro, 0)
    if (spent + amountMicro > leash.maxDailyMicro) {
      return {
        ok: false,
        reason: 'over_daily_leash',
        detail: `charge ${formatUSDX(amountMicro)} USDX would take the rolling 24h total past LAYERX_MAX_DAILY_USDX ${formatUSDX(leash.maxDailyMicro)} (spent ${formatUSDX(spent)})`,
      }
    }
  }
  return { ok: true }
}

// invokeLXP runs one priced invoke through the lxp/1 handshake.
async function invokeLXP(args, bearer) {
  const body = {
    operation: args.operation,
    args: args.args || {},
    quote_id: args.quote_id,
    idempotency_key: args.idempotency_key,
  }
  if (args.payment_rail) body.payment = { rail: args.payment_rail }
  const headers = {}
  if (bearer) headers.Authorization = `Bearer ${bearer}`
  let identity = null
  try {
    identity = loadIdentity()
    headers['X-Caller-DID'] = identity.did
  } catch {}

  const path = `/v1/invoke/${args.service_id}`
  const first = await rawFetch('POST', path, body, headers)
  if (first.status >= 200 && first.status < 300) return first.data
  const terms = first.status === 402 ? first.data?.lxp : null
  if (!terms) {
    const err = new Error(first.data?.message || first.data?.error || `HTTP ${first.status}`)
    err.status = first.status
    err.data = first.data
    throw err
  }
  if (!identity) throw new Error('lxp payment required but no executor key is available to sign with')

  const amountMicro = parseUSDXMicro(terms.amount_usdx)
  const leash = leashCheck(amountMicro)
  if (!leash.ok) {
    const err = new Error(`payment not signed (${leash.reason}): ${leash.detail}`)
    err.status = 402
    err.data = { reason: leash.reason, leash: leash.detail, terms }
    throw err
  }

  const payment = signPayment(terms, identity)
  const retry = await rawFetch('POST', path, body, {
    ...headers,
    'X-LayerX-Payment': encodePaymentHeader(payment),
  })
  if (retry.status >= 200 && retry.status < 300) {
    recordSpend(amountMicro, LEASH.journalPath)
    const receipt = decodeReceiptHeader(retry.headers.get('X-LayerX-Receipt'))
    return receipt ? { ...retry.data, layerx_receipt: receipt } : retry.data
  }
  const reason = retry.data?.reason || retry.data?.error || `HTTP ${retry.status}`
  const err = new Error(`lxp payment failed: ${reason}`)
  err.status = retry.status
  err.data = { reason, terms: retry.data?.lxp || terms }
  if (reason === 'insufficient_funds') {
    err.data.deposit_hint =
      `the paying DID ${identity.did} has insufficient USDX on LayerX (${terms.layerx}); ` +
      'fund it by depositing USDL to the LayerX vault bound to this DID (layerx_deposit tool -> vault address + did_claim), then retry'
  }
  throw err
}

async function callTool(name, args) {
  if (!BASE_URL) throw new Error('MATRIX_DEUS_URL not configured')
  let bearer = null
  if (WRITE_TOOLS.has(name)) {
    bearer = await mintWalletToken()
    if (!bearer && !AGENT.disabled) throw new Error('agent wallet token required for deus_invoke')
  }
  switch (name) {
    case 'deus_discover':
      return deusFetch('POST', '/v1/discover', {
        query: args.query || '',
        filters: args.filters || {},
        limit: args.limit || 10,
      })
    case 'deus_get_service':
      return deusFetch('GET', `/v1/services/${args.service_id}`, null)
    case 'deus_quote':
      return deusFetch('POST', `/v1/quote/${args.service_id}`, {
        operation: args.operation,
        estimated_units: args.estimated_units || '1',
      }, bearer || (await mintWalletToken()))
    case 'deus_invoke':
      return invokeLXP(args, bearer)
    case 'deus_invocation_status':
      return deusFetch('GET', `/v1/invocations/${args.invocation_id}`, null, bearer)
    case 'deus_my_spend':
      return { note: 'spend summary endpoint pending', invocations: [] }
    default:
      throw new Error(`unknown tool ${name}`)
  }
}

const handlers = {
  initialize: () => ({
    protocolVersion: PROTOCOL_VERSION,
    capabilities: { tools: {} },
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async ({ name, arguments: args }) => {
    if (!TOOL_SET.has(name)) return errResult(name, 'unknown tool')
    try {
      const data = await callTool(name, args || {})
      return okResult(data)
    } catch (err) {
      return errResult(name, err?.message ?? String(err), { status: err?.status, detail: err?.data })
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
    try { req = JSON.parse(line) } catch (err) {
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
}

function runSelftest() {
  console.log(`deus: ${tools.length} tools (remote=${BASE_URL})`)
  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.MATRIX_DEUS_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  const files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  let checked = 0
  let drift = false
  for (const file of files) {
    const doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    const server = (doc.servers || []).find((s) => s.alias === 'deus')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`deus FAIL: ${file} drifts`)
      if (bridgeOnly.length) console.error(`  bridge only: ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest only: ${manifestOnly.join(', ')}`)
    } else {
      console.log(`deus: ${file} matches`)
    }
  }
  if (checked === 0) {
    console.error('deus SELFTEST FAILED: no manifest declares deus server')
    process.exit(1)
  }
  if (drift) process.exit(1)
  console.log('deus OK')
  process.exit(0)
}

// runVectors computes preimages + signed payments for cross-implementation
// lockstep vectors (stdin JSON {seed, cases:[{kind,...}]}) so the Go side
// (deus/pkg/lxp) can assert byte-identity and verify the ed25519 signatures.
async function runVectors() {
  const chunks = []
  for await (const c of process.stdin) chunks.push(c)
  const input = JSON.parse(Buffer.concat(chunks).toString('utf8'))
  const seed = Buffer.from(input.seed, 'hex')
  const privateKey = createPrivateKey({ key: Buffer.concat([ED25519_PKCS8_PREFIX, seed]), format: 'der', type: 'pkcs8' })
  const spki = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
  const pubHex = Buffer.from(spki.subarray(spki.length - 32)).toString('hex')
  const identity = { did: `did:matrix:${input.label}:${pubHex.slice(0, 16)}`, pubHex, privateKey }
  const out = input.cases.map((terms) => {
    const payment = signPayment(terms, identity)
    const preimage = payment.mode === 'hold'
      ? holdPreimage(payment, terms.ttl_s || 120, terms.captor_did || '')
      : payPreimage(payment)
    return { preimage, payment, header: encodePaymentHeader(payment) }
  })
  process.stdout.write(JSON.stringify({ did: identity.did, public_key: pubHex, results: out }))
}

const invokedDirectly = (() => {
  if (!process.argv[1]) return false
  try {
    return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url))
  } catch {
    return false
  }
})()

if (process.argv.includes('--selftest')) {
  runSelftest()
} else if (process.argv.includes('--vectors')) {
  runVectors()
} else if (invokedDirectly) {
  startStdioServer()
}
