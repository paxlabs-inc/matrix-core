#!/usr/bin/env node
// deus — MCP stdio proxy bridging Matrix agents to the Deus gateway HTTP API.
// Mirrors tools/browser/browser.mjs: local tools/list, lazy remote on tools/call.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
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

async function deusFetch(method, path, body, bearer) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  const headers = { Accept: 'application/json', 'Content-Type': 'application/json' }
  if (bearer) headers.Authorization = `Bearer ${bearer}`
  try {
    const res = await fetch(`${BASE_URL}${path}`, {
      method,
      headers,
      body: body != null ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
    clearTimeout(timer)
    const raw = await res.text()
    let data
    try { data = raw ? JSON.parse(raw) : null } catch { data = { raw } }
    if (!res.ok) {
      const err = new Error(data?.message || data?.error || `HTTP ${res.status}`)
      err.status = res.status
      err.data = data
      throw err
    }
    return data
  } catch (e) {
    clearTimeout(timer)
    throw e
  }
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
      return deusFetch('POST', `/v1/invoke/${args.service_id}`, {
        operation: args.operation,
        args: args.args || {},
        quote_id: args.quote_id,
        idempotency_key: args.idempotency_key,
        payment: { rail: args.payment_rail || 'direct' },
      }, bearer)
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

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
