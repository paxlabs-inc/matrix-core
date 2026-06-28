// kindle bridge — chain access + the shared embedded-wallet write lane.
//
// READS: typed eth_call helpers bound to the kindle chain-125 RPC.
// WRITES: there is exactly ONE signing path — the Paxeer embedded wallet
// (connect.paxportwallet.com /v1/agent/* lane). We REUSE tools/paxeer/lib so
// kindle never introduces a second custody/signing surface (spec task 1.4):
//   - abi/keccak  : pure, dependency-free ABI codec + keccak256 selectors.
//   - rpc         : eth_call / JSON-RPC (we pass the kindle RPC url explicitly).
//   - net         : http + base-unit <-> human conversion.
//   - wallet      : /v1/agent/send sign+broadcast (network-side policy/leash).
// The EVM key stays server-side; kindle only encodes calldata and forwards it.

import { encodeCall, decode, encode } from '../../paxeer/lib/abi.mjs'
import { selector } from '../../paxeer/lib/keccak.mjs'
import { rpc as rawRpc } from '../../paxeer/lib/rpc.mjs'
import { toBaseUnits, fromBaseUnits } from '../../paxeer/lib/net.mjs'
import * as wallet from '../../paxeer/lib/wallet.mjs'
import { ENDPOINTS, CHAIN, TOKENS, resolveToken } from './config.mjs'

export { encodeCall, decode, encode, selector, toBaseUnits, fromBaseUnits, wallet }

const MAX_UINT256 = (1n << 256n) - 1n

// Some views read through chain services that are not uniformly available
// across the load-balanced public RPC, so an eth_call can transiently fail with
// an Internal error on one node yet succeed on the next. We retry such failures
// a few times (a new node may serve it); a clean revert/timeout still surfaces.
const RETRYABLE = /\b(internal|temporarily|timed out|timeout|503|502|429|connection|fetch failed)\b/i

async function withRetry(fn, tries = 4) {
  let last
  for (let i = 0; i < tries; i++) {
    try {
      return await fn()
    } catch (e) {
      last = e
      if (e && e.status === 403) throw e
      if (!RETRYABLE.test(String(e && e.message))) throw e
      await new Promise((r) => setTimeout(r, 150 * (i + 1)))
    }
  }
  throw last
}

// One JSON-RPC call against the kindle chain-125 RPC (with transient retry).
export const rpc = (method, params = []) => withRetry(() => rawRpc(method, params, ENDPOINTS.rpc))

// Read-only eth_call against the kindle RPC.
export async function ethCall(to, data, block = 'latest') {
  const d = String(data || '0x')
  return rpc('eth_call', [{ to, data: d.startsWith('0x') ? d : '0x' + d }, block])
}

// Encode a method, eth_call it, decode the outputs. signature e.g.
// "getAccumulatedFees(address)" ; outputs e.g. ["uint256"]. A single output is
// returned unwrapped; multiple are returned as an array.
export async function callMethod(to, signature, args = [], outputs = [], block = 'latest') {
  const raw = await ethCall(to, encodeCall(signature, args), block)
  if (!outputs.length) return raw
  if (!raw || raw === '0x') return null
  const decoded = decode(outputs, raw)
  return decoded.length === 1 ? decoded[0] : decoded
}

// decimalsFor — resolve a token's EXACT decimals. Known registry tokens use
// their pinned decimals; otherwise read ERC-20 decimals() on-chain. Never
// defaults silently to 18 (spec req.11 ac_2/ac_4): throws if it cannot resolve.
export async function decimalsFor(tokenRef) {
  const t = resolveToken(tokenRef)
  if (t && typeof t.decimals === 'number') return t.decimals
  const addr = (t && t.address) || (/^0x[0-9a-fA-F]{40}$/.test(String(tokenRef)) ? String(tokenRef) : null)
  if (!addr) throw new Error(`kindle: cannot resolve token "${tokenRef}"`)
  const d = await callMethod(addr, 'decimals()', [], ['uint8'])
  if (d == null) throw new Error(`kindle: token ${addr} did not report decimals()`)
  return Number(d)
}

// tokenAddress — 0x address for a symbol/address ref (PAX native -> null).
export function tokenAddress(tokenRef) {
  const t = resolveToken(tokenRef)
  if (t && t.native) return null
  if (t && t.address) return t.address
  if (/^0x[0-9a-fA-F]{40}$/.test(String(tokenRef))) return String(tokenRef)
  return null
}

export function explorerTx(hash) {
  return `${ENDPOINTS.explorer}/tx/${hash}`
}

// ── ERC-20 helpers ───────────────────────────────────────────────────────────
export async function allowance(token, owner, spender) {
  const v = await callMethod(token, 'allowance(address,address)', [owner, spender], ['uint256'])
  return BigInt(v ?? '0')
}

export async function erc20Balance(token, owner) {
  const v = await callMethod(token, 'balanceOf(address)', [owner], ['uint256'])
  return BigInt(v ?? '0')
}

export function approveCalldata(spender, amount) {
  return encodeCall('approve(address,uint256)', [spender, amount.toString()])
}

export const MAX_APPROVAL = MAX_UINT256

// ── The single signing path ───────────────────────────────────────────────────
// agentAddress — the per-user embedded-wallet address (provisions on first use).
export const agentAddress = () => wallet.address()
export const walletConfigured = () => wallet.isConfigured()

// sendTx — sign+broadcast one tx through the embedded wallet (network-side
// leash/policy enforcement; a 403 surfaces as a structured denial upstream).
export async function sendTx(tx) {
  const r = await wallet.send({ chainId: CHAIN.id, ...tx })
  return { ...r, explorer: r && r.tx_hash ? explorerTx(r.tx_hash) : undefined }
}

export { CHAIN, ENDPOINTS, TOKENS }
