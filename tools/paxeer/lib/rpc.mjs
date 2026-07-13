// Direct node access — EVM JSON-RPC reads + typed eth_call helpers.

import { ENDPOINTS, LIMITS } from './config.mjs'
import { httpPost } from './net.mjs'
import { encodeCall, decode } from './abi.mjs'

let _id = 0

// One JSON-RPC call against the Paxeer EVM node. Read-only by construction —
// this bridge never exposes eth_sendRawTransaction here; signing goes through
// the embedded-wallet REST surface (see wallet.mjs).
export async function rpc(method, params = [], url = ENDPOINTS.rpc) {
  // JSON-RPC requires `params` to be an array (or omitted). The planner
  // sometimes hands rpc_call a scalar (e.g. "latest"), an object, or a
  // JSON-encoded string ('["latest", false]') — all of which the node
  // rejects with "non-array args". Normalise here so a slightly-off call
  // shape still works instead of hard-failing the run.
  params = normalizeParams(params)
  const res = await httpPost(url, { jsonrpc: '2.0', id: ++_id, method, params }, { timeoutMs: LIMITS.rpcTimeoutMs })
  if (res && res.error) throw new Error(`rpc ${method}: ${res.error.message || JSON.stringify(res.error)}`)
  return res ? res.result : null
}

// normalizeParams coerces a params value into the array JSON-RPC expects.
function normalizeParams(params) {
  if (Array.isArray(params)) return params
  if (params == null) return []
  if (typeof params === 'string') {
    const s = params.trim()
    if (s.startsWith('[')) {
      try {
        const parsed = JSON.parse(s)
        if (Array.isArray(parsed)) return parsed
      } catch { /* fall through to wrapping */ }
    }
  }
  return [params]
}

export const hexToInt = (h) => (h == null ? null : typeof h === 'string' && h.startsWith('0x') ? parseInt(h, 16) : Number(h))
export const hexToBig = (h) => (h == null ? null : BigInt(h))

// Raw read-only eth_call. Does NOT send a transaction or mutate state.
export async function ethCall(to, data, block = 'latest') {
  const d = String(data || '0x')
  return rpc('eth_call', [{ to, data: d.startsWith('0x') ? d : '0x' + d }, block])
}

// Encode a method call, eth_call it, and (optionally) decode the outputs.
// signature: "getSpotPrice()" ; outputs: ["uint256"].
export async function callMethod(to, signature, args = [], outputs = [], block = 'latest') {
  const raw = await ethCall(to, encodeCall(signature, args), block)
  if (!outputs.length) return raw
  if (!raw || raw === '0x') return null
  const decoded = decode(outputs, raw)
  return decoded.length === 1 ? decoded[0] : decoded
}

export const getBalance = (addr, block = 'latest') => rpc('eth_getBalance', [addr, block])
export const blockNumber = () => rpc('eth_blockNumber')
export const gasPrice = () => rpc('eth_gasPrice')
export const getTransactionByHash = (hash) => rpc('eth_getTransactionByHash', [hash])
export const getTransactionReceipt = (hash) => rpc('eth_getTransactionReceipt', [hash])
export const getTransactionCount = (addr, block = 'latest') => rpc('eth_getTransactionCount', [addr, block])
export const getCode = (addr, block = 'latest') => rpc('eth_getCode', [addr, block])
export const chainId = () => rpc('eth_chainId')
export const peerCount = () => rpc('net_peerCount')
export const syncing = () => rpc('eth_syncing')

// ── Confirmation helpers ─────────────────────────────────────────────────────
// Client-side receipt waiting with a structured (never-throws) outcome, so the
// legacy one-shot contract_write path can confirm robustly too. The durable
// action lane confirms server-side; this is for callers driving raw sends.

const _sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// Poll for a receipt until it reaches `confirmations` blocks deep, reverts, or
// the timeout elapses. Returns one of:
//   { state:'confirmed', receipt }  { state:'reverted', receipt }
//   { state:'pending' }  (broadcast + still known to the node)
//   { state:'timeout' }  (gave up; the tx may yet mine — do NOT blindly resend)
export async function waitForReceipt(hash, { confirmations = 1, timeoutMs = 90000, pollMs = 1500 } = {}) {
  if (!hash) throw new Error('waitForReceipt: hash is required')
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const receipt = await getTransactionReceipt(hash).catch(() => null)
    if (receipt && receipt.blockNumber != null) {
      const success = receipt.status === '0x1' || receipt.status === 1 || receipt.status === true
      if (!success) return { state: 'reverted', receipt }
      if (confirmations <= 1) return { state: 'confirmed', receipt }
      const head = hexToInt(await blockNumber().catch(() => null))
      const mined = hexToInt(receipt.blockNumber)
      if (head != null && mined != null && head - mined + 1 >= confirmations) {
        return { state: 'confirmed', receipt }
      }
    }
    if (Date.now() >= deadline) {
      if (!receipt) return (await isTxKnown(hash)) ? { state: 'pending' } : { state: 'timeout' }
      return { state: 'timeout' }
    }
    await _sleep(pollMs)
  }
}

// True when the node still knows the tx (mined OR queued in the mempool).
export async function isTxKnown(hash) {
  const tx = await getTransactionByHash(hash).catch(() => null)
  if (tx) return true
  const r = await getTransactionReceipt(hash).catch(() => null)
  return !!r
}

// ERC-20 reads bundled (used by balance/portfolio tools).
export async function erc20(addr, account) {
  const [bal, dec, sym] = await Promise.allSettled([
    account ? callMethod(addr, 'balanceOf(address)', [account], ['uint256']) : Promise.resolve(null),
    callMethod(addr, 'decimals()', [], ['uint8']),
    callMethod(addr, 'symbol()', [], ['string']),
  ])
  return {
    address: addr,
    symbol: sym.status === 'fulfilled' ? sym.value : null,
    decimals: dec.status === 'fulfilled' ? Number(dec.value) : null,
    balance: bal.status === 'fulfilled' ? bal.value : null,
  }
}
