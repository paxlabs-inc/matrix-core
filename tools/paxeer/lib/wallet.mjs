// Embedded wallet REST client — the network-side custody + enforcement surface.
//
// Agents act on-chain by POSTing transactions here; the API server holds the
// key and signs. Because custody lives at the network layer, spend limits,
// allow-lists, and policy are enforced server-side on the wallet — not trusted
// to this client. Generic {to,data,value} means any precompile/contract call
// routes through it.
//
// AUTH — two lanes, agent-native preferred:
//   1. Agent-native (default in the daemon): the ed25519 executor key proves a
//      did:matrix:<label>:<keyfp> identity (lib/agentauth) and drives the
//      dedicated kind='agent' routes under /v1/agent/*.
//   2. Legacy human token (fallback): PAXEER_WALLET_TOKEN, or
//      PAXEER_WALLET_EMAIL + PAXEER_WALLET_PASSWORD (+ PAXEER_SUPABASE_ANON_KEY)
//      password grant, against the human /v1/wallet/* routes.

import { WALLET_API, CHAIN } from './config.mjs'
import { httpJson, httpPost } from './net.mjs'
import * as agent from './agentauth.mjs'

let _token = WALLET_API.token || null

function canRefresh() {
  return Boolean(WALLET_API.email && WALLET_API.password && WALLET_API.supabaseAnonKey)
}

function useAgent() {
  return agent.isAgentConfigured()
}

export function isConfigured() {
  return useAgent() || Boolean(WALLET_API.token || canRefresh())
}

async function getToken(force = false) {
  if (_token && !force) return _token
  if (WALLET_API.token && !force) {
    _token = WALLET_API.token
    return _token
  }
  if (canRefresh()) {
    const res = await httpPost(
      `${WALLET_API.supabaseUrl}/auth/v1/token?grant_type=password`,
      { email: WALLET_API.email, password: WALLET_API.password },
      { headers: { apikey: WALLET_API.supabaseAnonKey } },
    )
    if (!res || !res.access_token) throw new Error('paxeer wallet: password grant returned no access_token')
    _token = res.access_token
    return _token
  }
  if (WALLET_API.token) {
    _token = WALLET_API.token
    return _token
  }
  throw new Error(
    'paxeer wallet: no auth configured. Set PAXEER_WALLET_TOKEN, or ' +
      'PAXEER_WALLET_EMAIL + PAXEER_WALLET_PASSWORD (+ PAXEER_SUPABASE_ANON_KEY).',
  )
}

async function legacyCall(method, path, body, retry = true) {
  const token = await getToken()
  try {
    return await httpJson(method, `${WALLET_API.base}${path}`, {
      headers: { Authorization: `Bearer ${token}` },
      body,
    })
  } catch (e) {
    if (e.status === 401 && retry && canRefresh()) {
      await getToken(true)
      return legacyCall(method, path, body, false)
    }
    throw e
  }
}

// Normalize a tx request into the wire shape the API expects (decimal strings
// for value/gas, chainId default to Paxeer mainnet).
function serializeTx(tx = {}) {
  const out = {}
  if (tx.to !== undefined) out.to = tx.to
  if (tx.data !== undefined) out.data = tx.data
  if (tx.value !== undefined && tx.value !== null) out.value = String(tx.value)
  if (tx.gas !== undefined && tx.gas !== null) out.gas = String(tx.gas)
  if (tx.maxFeePerGas !== undefined) out.maxFeePerGas = String(tx.maxFeePerGas)
  if (tx.maxPriorityFeePerGas !== undefined) out.maxPriorityFeePerGas = String(tx.maxPriorityFeePerGas)
  if (tx.nonce !== undefined) out.nonce = tx.nonce
  out.chainId = tx.chainId ?? CHAIN.id
  return out
}

// GET .../me — returns {wallet,chain} (agent: wallet may be null pre-provision).
export async function me() {
  if (useAgent()) return agent.agentCall('GET', '/v1/agent/me')
  try {
    return await legacyCall('GET', '/v1/wallet/me')
  } catch (e) {
    if (e.status === 404) return null
    throw e
  }
}

// Auto-create the agent's wallet (idempotent find-or-create on the agent lane).
export const provision = () =>
  useAgent() ? agent.agentCall('POST', '/v1/agent/provision') : legacyCall('POST', '/v1/wallet/provision')

// Resolve {wallet,chain}, provisioning on first use.
export async function ensureWallet() {
  if (useAgent()) {
    let r = await agent.agentCall('GET', '/v1/agent/me')
    if (!r || !r.wallet) {
      await agent.agentCall('POST', '/v1/agent/provision')
      r = await agent.agentCall('GET', '/v1/agent/me')
    }
    return r
  }
  const existing = await me()
  if (existing) return existing
  await provision()
  return legacyCall('GET', '/v1/wallet/me')
}

export async function address() {
  const r = await ensureWallet()
  return r && r.wallet ? r.wallet.address : null
}

// sign + broadcast. Returns {tx_hash,address,chain_id}. Policy-gated on the
// agent lane (403 {error:CODE} when frozen / over a cap / read_only).
export const send = (tx) =>
  useAgent()
    ? agent.agentCall('POST', '/v1/agent/send', { tx: serializeTx(tx) })
    : legacyCall('POST', '/v1/wallet/send', { tx: serializeTx(tx) })

// sign without broadcasting. Returns {signed_tx,...}.
export const sign = (tx) =>
  useAgent()
    ? agent.agentCall('POST', '/v1/agent/sign', { tx: serializeTx(tx) })
    : legacyCall('POST', '/v1/wallet/sign', { tx: serializeTx(tx) })

// EIP-191 personal_sign. Returns {signature,address}.
export const signMessage = (message) =>
  useAgent()
    ? agent.agentCall('POST', '/v1/agent/sign-message', { message })
    : legacyCall('POST', '/v1/wallet/sign-message', { message })
