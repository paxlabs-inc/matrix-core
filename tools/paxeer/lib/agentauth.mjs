// Agent-native DID auth for the Paxeer embedded-wallet lane.
//
// The Matrix daemon's ed25519 executor key (the SAME key it signs envelopes
// with) IS the agent's identity. This module proves possession of that key to
// the wallet API via a challenge/verify handshake and exchanges it for a
// short-lived agent_token that authorizes the dedicated kind='agent' routes
// under /v1/agent/*. The EVM signing key stays server-side; this only proves
// "I am did:matrix:<label>:<keyfp>".
//
// Zero-dep: node:crypto (ed25519) + node:fs. Key format + DID derivation MUST
// match the Go daemon (executor/cmd/mcl-execute/identity.go): a 64-hex ed25519
// SEED on disk; DID = did:matrix:<label>:<hex(pubkey)[:16]>.

import { createPrivateKey, createPublicKey, sign as edSign } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { WALLET_API, AGENT_AUTH } from './config.mjs'
import { httpJson } from './net.mjs'

// Fixed PKCS8 DER wrapper for a raw 32-byte Ed25519 seed (RFC 8410), so a bare
// seed becomes a Node KeyObject without any external dependency.
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')

let _identity = null
let _token = null

function base() {
  return WALLET_API.base.replace(/\/v1$/, '')
}

function loadIdentity() {
  if (_identity) return _identity
  const raw = readFileSync(AGENT_AUTH.keyfile, 'utf8').trim()
  if (!/^[0-9a-fA-F]{64}$/.test(raw)) {
    throw new Error(`paxeer agent auth: ${AGENT_AUTH.keyfile} is not a 64-hex ed25519 seed`)
  }
  const seed = Buffer.from(raw, 'hex')
  const privateKey = createPrivateKey({
    key: Buffer.concat([ED25519_PKCS8_PREFIX, seed]),
    format: 'der',
    type: 'pkcs8',
  })
  const spki = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
  const pubHex = Buffer.from(spki.subarray(spki.length - 32)).toString('hex')
  const label = AGENT_AUTH.label || 'executor'
  _identity = { did: `did:matrix:${label}:${pubHex.slice(0, 16)}`, pubHex, privateKey, label }
  return _identity
}

export function isAgentConfigured() {
  if (AGENT_AUTH.disabled) return false
  try {
    loadIdentity()
    return true
  } catch {
    return false
  }
}

export function agentDid() {
  return loadIdentity().did
}

// challenge -> ed25519-sign the returned `message` -> verify -> cache token.
async function authenticate() {
  const id = loadIdentity()
  const ch = await httpJson('POST', `${base()}/v1/agent/auth/challenge`, { body: { did: id.did } })
  if (!ch || !ch.message || !ch.nonce) {
    throw new Error('paxeer agent auth: challenge returned no message/nonce')
  }
  const signature = edSign(null, Buffer.from(ch.message, 'utf8'), id.privateKey).toString('hex')
  const vr = await httpJson('POST', `${base()}/v1/agent/auth/verify`, {
    body: { did: id.did, public_key: id.pubHex, nonce: ch.nonce, signature },
  })
  if (!vr || !vr.token) throw new Error('paxeer agent auth: verify returned no token')
  _token = vr.token
  return _token
}

export async function getAgentToken(force = false) {
  if (_token && !force) return _token
  return authenticate()
}

// Authed call against the agent lane; re-auths once on a 401.
export async function agentCall(method, path, body, retry = true) {
  const token = await getAgentToken()
  try {
    return await httpJson(method, `${base()}${path}`, {
      headers: { Authorization: `Bearer ${token}` },
      body,
    })
  } catch (e) {
    if (e.status === 401 && retry) {
      await getAgentToken(true)
      return agentCall(method, path, body, false)
    }
    throw e
  }
}
