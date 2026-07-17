// paxeer-net tool registry + dispatch. Reads need no auth; writes route
// through the embedded wallet (network-side custody). A write tool degrades
// gracefully (returns an explanatory result) when no wallet auth is set.

import { CHAIN, ENDPOINTS, TOKENS, resolveToken, LIMITS } from './config.mjs'
import { ok, toBaseUnits, fromBaseUnits } from './net.mjs'
import * as rpc from './rpc.mjs'
import * as paxscan from './paxscan.mjs'
import * as markets from './markets.mjs'
import * as wallet from './wallet.mjs'
import * as pc from './precompiles.mjs'
import { encodeCall, decode } from './abi.mjs'

// READS-ONLY surface (PAXEER_READS_ONLY=1): expose the full chain DATA layer
// but NO signing/spending tools. This is the lane the Neo conversational agent
// gets — frozen-spec invariant i1 ("Neo never holds a signing key; all money
// crosses into MCL"): Neo reads the chain directly, but value-moving actions go
// through core_execute to the MCL daemon. When the flag is UNSET (the daemon's
// default), behaviour is byte-identical to before: the full read+write registry.
const READS_ONLY = process.env.PAXEER_READS_ONLY === '1'
const WRITE_TOOL_NAMES = new Set([
  'wallet_info', 'sign_message', 'transfer', 'approve',
  'contract_write',
  // durable high-level intents (server-owned approve→call→verify). wallet_action
  // is a read (polls status) and is intentionally NOT listed here.
  'wallet_layerx_deposit', 'wallet_allowance_and_call',
])

function unitsFor(tokenRef) {
  const t = resolveToken(tokenRef)
  return t ? t.decimals : 18
}
function addressFor(tokenRef) {
  const t = resolveToken(tokenRef)
  if (t && t.address) return t.address
  if (/^0x[0-9a-fA-F]{40}$/.test(String(tokenRef))) return tokenRef
  return null
}
async function resolveAddr(a) {
  if (a && /^0x[0-9a-fA-F]{40}$/.test(String(a))) return a
  return wallet.address()
}
function guardSpend(valueWei) {
  const max = BigInt(LIMITS.maxSpendWei || '0')
  if (max > 0n && BigInt(valueWei || '0') > max) {
    throw new Error(`spend ${valueWei} wei exceeds PAXEER_MAX_SPEND_WEI ${LIMITS.maxSpendWei}`)
  }
}
async function writeTx(tx, extra = {}) {
  if (!wallet.isConfigured()) {
    return ok({ ok: false, error: 'wallet not configured', hint: 'set PAXEER_WALLET_TOKEN or PAXEER_WALLET_EMAIL+PAXEER_WALLET_PASSWORD', intended_tx: tx })
  }
  guardSpend(tx.value)
  const r = await wallet.send(tx)
  return ok({ ok: true, ...r, explorer: `${ENDPOINTS.paxscan}/tx/${r.tx_hash}`, ...extra })
}

// Shape a durable-action creation as a tool result. When `wait` is truthy we
// block until the action reaches a terminal state (final.ok tells the outcome);
// otherwise we return immediately with the action_id + idempotency_key so the
// agent can poll wallet_action. The idempotency_key is always surfaced so a
// retry reuses it (same key → same action, never a second nonce / deposit).
async function finishAction(name, created, wait) {
  const actionId = created && created.action_id
  if ((wait === true || wait === 'true') && actionId) {
    const final = await wallet.awaitAction(actionId)
    return ok({ ok: final && final.ok !== false, tool: name, idempotency_key: created.idempotency_key, action_id: actionId, action: final })
  }
  return ok({ ok: true, tool: name, idempotency_key: created && created.idempotency_key, action_id: actionId, action: created })
}

// decodeHexInt parses a 0x-prefixed hex quantity into a BigInt, or null
// when v is not a hex string.
function decodeHexInt(v) {
  if (typeof v !== 'string' || !v.startsWith('0x')) return null
  try { return BigInt(v) } catch { return null }
}

// decodeRpcResult returns a human-readable companion for well-known
// JSON-RPC result shapes (blocks, transactions, scalar quantities),
// or null when there is nothing useful to decode. It NEVER mutates the
// raw result — the raw value is always returned faithfully alongside.
//
// WHY: eth_* results encode every numeric field as opaque hex (a block's
// `timestamp` is e.g. "0x6a21078d"). Handed that raw, the answer-composer
// LLM tries to convert hex→decimal→date by hand and spirals into a wrong,
// rambling answer. Decoding deterministically here — especially the block
// timestamp into ISO-8601 — gives the agent ground truth it can state
// verbatim, with no arithmetic to fumble. Same principle as decoding raw
// wei into token decimals.
function decodeRpcResult(result) {
  if (result == null) return null
  // Scalar quantity (eth_blockNumber, eth_gasPrice, eth_chainId, …).
  if (typeof result === 'string') {
    const n = decodeHexInt(result)
    return n == null ? null : { value_int: n.toString() }
  }
  if (typeof result !== 'object') return null
  const out = {}
  const intFields = ['number', 'timestamp', 'gasLimit', 'gasUsed', 'baseFeePerGas',
    'size', 'nonce', 'gas', 'gasPrice', 'value', 'blockNumber', 'transactionIndex',
    'chainId', 'cumulativeGasUsed', 'effectiveGasPrice', 'status', 'blobGasUsed', 'excessBlobGas']
  for (const k of intFields) {
    if (k in result) {
      const n = decodeHexInt(result[k])
      if (n != null) out[k + '_int'] = n.toString()
    }
  }
  // Block / tx timestamp → human-readable UTC + age, the field that most
  // often needs interpreting.
  const ts = decodeHexInt(result.timestamp)
  if (ts != null) {
    out.timestamp_iso = new Date(Number(ts) * 1000).toISOString()
    out.age_seconds = Math.max(0, Math.floor(Date.now() / 1000) - Number(ts))
  }
  return Object.keys(out).length ? out : null
}

// ── dispatch ────────────────────────────────────────────────────────────────
export async function dispatch(name, args = {}) {
  // Defence-in-depth: on the read-only surface, refuse any signing/spending
  // tool even if one were somehow requested (they are not advertised either).
  if (READS_ONLY && WRITE_TOOL_NAMES.has(name)) {
    return ok({
      ok: false,
      tool: name,
      error: 'read-only paxeer surface: signing/spending tools are disabled here',
      hint: 'money & signature actions run through the secure execution path (core_execute), not this read-only bridge',
    })
  }
  switch (name) {
    // —— direct node RPC ——
    case 'rpc_call': {
      const result = await rpc.rpc(args.method, args.params || [])
      const decoded = decodeRpcResult(result)
      return ok(decoded ? { tool: name, result, decoded } : { tool: name, result })
    }
    case 'eth_call':
      return ok({ tool: name, result: await rpc.ethCall(args.to, args.data || '0x', args.block || 'latest') })
    case 'contract_read': {
      const out = await rpc.callMethod(args.to, args.signature, args.args || [], args.outputs || [], args.block || 'latest')
      return ok({ tool: name, to: args.to, signature: args.signature, result: out })
    }
    case 'encode_call':
      // Pure ABI encode (no network, no send). Used to build or inspect
      // calldata before a contract_write.
      return ok({ tool: name, to: args.to, signature: args.signature, data: encodeCall(args.signature, args.args || []) })
    case 'chain_info': {
      const syncing = rpc.syncing()
        .then((value) => ({ value: value === false ? false : value }))
        .catch((err) => {
          const message = err?.message ?? String(err)
          if (!/method not found|method .* (?:unsupported|not supported)|does not exist|-32601/i.test(message)) {
            throw err
          }
          return {
            value: null,
            unavailable: true,
            note: 'eth_syncing is not supported by this RPC; chain id and head block remain available',
          }
        })
      const [bn, cid, sync] = await Promise.all([rpc.blockNumber(), rpc.chainId(), syncing])
      return ok({
        tool: name,
        chain: CHAIN,
        blockNumber: rpc.hexToInt(bn),
        chainId: rpc.hexToInt(cid),
        syncing: sync.value,
        capabilities: { eth_syncing: sync.unavailable ? 'unavailable' : 'available' },
        ...(sync.note ? { capabilityNote: sync.note } : {}),
        rpc: ENDPOINTS.rpc,
      })
    }
    case 'get_balance': {
      const addr = await resolveAddr(args.address)
      const wei = await rpc.getBalance(addr)
      return ok({ tool: name, address: addr, wei: rpc.hexToBig(wei)?.toString(), pax: fromBaseUnits(rpc.hexToBig(wei) ?? 0n, 18) })
    }
    case 'token_balance': {
      const addr = await resolveAddr(args.address)
      const tokenAddr = addressFor(args.token)
      if (!tokenAddr) throw new Error(`unknown token ${args.token}`)
      const info = await rpc.erc20(tokenAddr, addr)
      return ok({ tool: name, account: addr, ...info, balanceFormatted: info.balance != null && info.decimals != null ? fromBaseUnits(info.balance, info.decimals) : null })
    }

    // —— PaxScan (Blockscout) ——
    case 'paxscan_get':
      return ok({ tool: name, result: await paxscan.get(args.path, args.params) })
    case 'address_overview': {
      const [info, counters, balances] = await Promise.allSettled([
        paxscan.address(args.address), paxscan.addressCounters(args.address), paxscan.addressTokenBalances(args.address),
      ])
      return ok({ tool: name, address: args.address,
        info: info.status === 'fulfilled' ? info.value : { error: String(info.reason) },
        counters: counters.status === 'fulfilled' ? counters.value : null,
        tokenBalances: balances.status === 'fulfilled' ? balances.value : null })
    }
    case 'address_transactions':
      return ok({ tool: name, result: await paxscan.addressTransactions(args.address, args.params) })
    case 'tx':
      return ok({ tool: name, result: await paxscan.transaction(args.hash) })
    case 'token_info': {
      const [tok, holders] = await Promise.allSettled([paxscan.token(args.token), paxscan.tokenHolders(args.token)])
      const token = tok.status === 'fulfilled' ? tok.value : null
      const holderData = holders.status === 'fulfilled' ? holders.value : null
      // PaxScan returns total_supply and each holder's value as RAW base
      // units (wei-like integers). Divide by the token's decimals so the
      // agent reports human-readable amounts instead of giant integers.
      // Raw fields are kept; *_formatted are added (cf. balanceFormatted).
      const dec = Number(token?.decimals)
      const decimals = Number.isFinite(dec) ? dec : 18
      const fmt = (v) => {
        if (v == null || v === '') return null
        try { return fromBaseUnits(v, decimals) } catch { return null }
      }
      if (token && token.total_supply != null) token.total_supply_formatted = fmt(token.total_supply)
      if (holderData && Array.isArray(holderData.items)) {
        for (const h of holderData.items) {
          if (h && h.value != null) h.value_formatted = fmt(h.value)
        }
      }
      return ok({ tool: name, decimals, token, holders: holderData })
    }
    case 'search':
      return ok({ tool: name, result: await paxscan.search(args.q) })
    case 'network_stats':
      return ok({ tool: name, result: await paxscan.stats() })

    // —— markets / price ——
    case 'price':
      return ok({ tool: name, result: await markets.price(args.symbol || 'pax') })

    // —— wallet / identity ——
    case 'wallet_info': {
      if (!wallet.isConfigured()) return ok({ ok: false, error: 'wallet not configured' })
      const r = await wallet.ensureWallet()
      return ok({ ok: true, address: r?.wallet?.address, chainId: r?.wallet?.chain_id, chain: r?.chain })
    }
    case 'sign_message': {
      if (!wallet.isConfigured()) return ok({ ok: false, error: 'wallet not configured' })
      return ok({ ok: true, ...(await wallet.signMessage(args.message)) })
    }

    // —— writes: payments ——
    case 'transfer': {
      // Optional explicit nonce + fee ceiling: stuck-tx replacement (fill a
      // NONCE GAP with a 0-value self-send at the wedged nonce, fees bumped
      // above the stuck tx so the node accepts the replacement).
      const overrides = {}
      if (args.nonce !== undefined && args.nonce !== null && String(args.nonce) !== '') {
        overrides.nonce = Number(args.nonce)
        if (!Number.isInteger(overrides.nonce) || overrides.nonce < 0) throw new Error(`invalid nonce ${args.nonce}`)
      }
      if (args.max_fee_gwei !== undefined && args.max_fee_gwei !== null && String(args.max_fee_gwei) !== '') {
        overrides.maxFeePerGas = toBaseUnits(args.max_fee_gwei, 9)
        overrides.maxPriorityFeePerGas = overrides.maxFeePerGas
      }
      const isNative = !args.token || String(args.token).toUpperCase() === 'PAX'
      if (isNative) {
        const value = toBaseUnits(args.amount, 18)
        return writeTx({ to: args.to, value, ...overrides }, { kind: 'native_transfer', to: args.to, amount: args.amount })
      }
      const tokenAddr = addressFor(args.token)
      if (!tokenAddr) throw new Error(`unknown token ${args.token}`)
      const base = toBaseUnits(args.amount, unitsFor(args.token))
      return writeTx({ ...pc.erc20.transfer(tokenAddr, args.to, base), ...overrides }, { kind: 'erc20_transfer', token: tokenAddr, to: args.to, amount: args.amount })
    }
    case 'approve': {
      const tokenAddr = addressFor(args.token)
      if (!tokenAddr) throw new Error(`unknown token ${args.token}`)
      const base = args.amount === 'max' ? ((1n << 256n) - 1n).toString() : toBaseUnits(args.amount, unitsFor(args.token))
      return writeTx(pc.erc20.approve(tokenAddr, args.spender, base), { kind: 'approve', token: tokenAddr, spender: args.spender })
    }

    // —— writes: generic contract call ——
    case 'contract_write': {
      const data = args.data ?? encodeCall(args.signature, args.args || [])
      const value = args.value != null ? toBaseUnits(args.value, 18) : (args.valueWei ?? undefined)
      return writeTx({ to: args.to, data, value, gas: args.gas }, { kind: 'contract_write', to: args.to, signature: args.signature })
    }

    // —— writes: durable high-level intents (server owns approve→call→verify) ——
    case 'wallet_layerx_deposit': {
      if (!wallet.isConfigured()) return ok({ ok: false, error: 'wallet not configured' })
      const created = await wallet.createLayerxDeposit({
        amount: args.amount,
        didClaim: args.did_claim,
        idempotencyKey: args.idempotency_key,
      })
      return finishAction(name, created, args.wait)
    }
    case 'wallet_allowance_and_call': {
      if (!wallet.isConfigured()) return ok({ ok: false, error: 'wallet not configured' })
      const created = await wallet.createAllowanceAndCall({
        token: args.token,
        amount: args.amount,
        spender: args.spender,
        contract: args.contract,
        method: args.method,
        args: args.args || [],
        idempotencyKey: args.idempotency_key,
      })
      return finishAction(name, created, args.wait)
    }
    case 'wallet_action': {
      if (!wallet.isConfigured()) return ok({ ok: false, error: 'wallet not configured' })
      if (!args.action_id) return ok({ ok: false, error: 'action_id is required' })
      const state = args.wait === true || args.wait === 'true'
        ? await wallet.awaitAction(args.action_id)
        : await wallet.getAction(args.action_id)
      return ok({ ok: true, tool: name, action_id: args.action_id, action: state })
    }

    default:
      throw new Error(`unknown tool: ${name}`)
  }
}

// ── tool descriptors (advertised to the MCP client) ───────────────────────
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })

const ALL_TOOLS = [
  // reads — node
  { name: 'rpc_call', description: 'Direct EVM JSON-RPC call (read-only). args: method, params[].', inputSchema: A({ method: S('JSON-RPC method e.g. eth_getBlockByNumber'), params: { type: 'array' } }, ['method']) },
  { name: 'eth_call', description: 'Read-only eth_call against a contract. Does NOT send a tx.', inputSchema: A({ to: S('contract address'), data: S('0x calldata'), block: S('block tag') }, ['to']) },
  { name: 'contract_read', description: 'Encode+eth_call a method and decode outputs. args: to, signature e.g. "balanceOf(address)", args[], outputs[] e.g. ["uint256"].', inputSchema: A({ to: S('contract'), signature: S('method signature'), args: { type: 'array' }, outputs: { type: 'array' }, block: S('block tag') }, ['to', 'signature']) },
  { name: 'encode_call', description: 'Pure ABI-encode a method call to 0x calldata (no network, no send). Use to build or inspect calldata. args: signature e.g. "transfer(address,uint256)", args[], to? (echoed back).', inputSchema: A({ signature: S('method signature'), args: { type: 'array' }, to: S('optional contract address to echo') }, ['signature']) },
  { name: 'chain_info', description: 'Paxeer chain id, head block, sync status, RPC URL.', inputSchema: A({}) },
  { name: 'get_balance', description: 'Native PAX balance of an address (defaults to the agent wallet).', inputSchema: A({ address: S('0x address; optional') }) },
  { name: 'token_balance', description: 'ERC-20 balance + symbol/decimals. args: token (symbol or 0x), address?', inputSchema: A({ token: S('symbol or 0x'), address: S('holder; optional') }, ['token']) },
  // reads — paxscan
  { name: 'paxscan_get', description: 'Generic PaxScan/Blockscout v2 GET passthrough. args: path e.g. "/blocks", params{}.', inputSchema: A({ path: S('/api/v2 path'), params: { type: 'object' } }, ['path']) },
  { name: 'address_overview', description: 'PaxScan address info + counters + token balances in one call.', inputSchema: A({ address: S('0x address') }, ['address']) },
  { name: 'address_transactions', description: 'PaxScan transaction list for an address.', inputSchema: A({ address: S('0x address'), params: { type: 'object' } }, ['address']) },
  { name: 'tx', description: 'PaxScan transaction detail by hash.', inputSchema: A({ hash: S('0x tx hash') }, ['hash']) },
  { name: 'token_info', description: 'PaxScan token metadata + top holders.', inputSchema: A({ token: S('token 0x address') }, ['token']) },
  { name: 'search', description: 'PaxScan global search (addresses, tokens, txs, blocks).', inputSchema: A({ q: S('query') }, ['q']) },
  { name: 'network_stats', description: 'PaxScan network stats (gas, market, tx counts).', inputSchema: A({}) },
  // reads — markets
  { name: 'price', description: 'Off-chain price for PAX or a bridged major. args: symbol (pax|sol|eth|bnb|sid).', inputSchema: A({ symbol: S('pax|sol|eth|bnb|sid') }) },
  // identity / wallet
  { name: 'wallet_info', description: 'Resolve the agent embedded-wallet address + chain (provisions on first use).', inputSchema: A({}) },
  { name: 'sign_message', description: 'EIP-191 personal_sign a message with the agent wallet (proof of identity).', inputSchema: A({ message: S('message to sign') }, ['message']) },
  // writes — payments
  { name: 'transfer', description: 'Send PAX (token omitted/"PAX") or an ERC-20 to a plain address. args: to, amount (human), token?, nonce?, max_fee_gwei? ONLY for direct wallet-to-wallet sends — NEVER to fund a deposit-style protocol (LayerX, vaults, bridges): those credit from their own deposit function, so a bare transfer to their address strands the funds. LayerX funding starts with layerx_deposit; contract deposits go through contract_write. nonce + max_fee_gwei are for stuck-tx replacement: a 0-amount self-send at the wedged nonce with fees above the stuck tx fills a NONCE GAP and unblocks the queue.', inputSchema: A({ to: S('recipient 0x'), amount: S('human amount e.g. "1.5"'), token: S('symbol or 0x; omit for PAX'), nonce: S('optional explicit nonce — replace/fill a stuck (unmined) tx at that nonce'), max_fee_gwei: S('optional max fee in gwei — set above the stuck tx when replacing') }, ['to', 'amount']) },
  { name: 'approve', description: 'ERC-20 approve. args: token, spender, amount (human or "max").', inputSchema: A({ token: S('symbol or 0x'), spender: S('0x'), amount: S('human or "max"') }, ['token', 'spender', 'amount']) },
  // writes — generic
  { name: 'contract_write', description: 'Sign and send a caller-specified contract write via the wallet. Provide signature+args (encoded for you) or raw data. args: to, signature?, args[]?, data?, value? (human PAX).', inputSchema: A({ to: S('contract 0x address'), signature: S('method signature'), args: { type: 'array' }, data: S('0x calldata (overrides signature)'), value: S('human PAX to attach'), gas: S('gas limit') }, ['to']) },
  // writes — durable high-level intents (one call; the WALLET owns the whole
  // approve→confirm→call→confirm→verify sequence, is idempotent, and survives a
  // disconnect). Prefer these over hand-rolling approve + contract_write.
  { name: 'wallet_layerx_deposit', description: 'Fund your LayerX USDX balance in ONE durable, idempotent call: the wallet does approve(USDL→vault) then depositUSDL(amount, did_claim), confirms each leg, and verifies the credit — surviving disconnects. Get did_claim (bytes32) + amount FIRST from layerx_deposit. amount is a human USDL decimal (e.g. "250"). Returns an action_id; poll wallet_action (or pass wait=true to block until terminal). Reuse idempotency_key on retry — the same key returns the SAME action (never a second deposit). This SUPERSEDES hand-rolling approve + contract_write for LayerX funding.', inputSchema: A({ amount: S('human USDL decimal, e.g. "250"'), did_claim: S('bytes32 did_claim from layerx_deposit'), idempotency_key: S('optional; reuse across retries. auto-generated + returned if omitted'), wait: S('optional "true" to block until the action is terminal') }, ['amount', 'did_claim']) },
  { name: 'wallet_allowance_and_call', description: 'Generic durable approve-then-call in ONE idempotent action: approve(token→spender, amount) then contract.method(args), each leg confirmed by the wallet. amount + numeric args are RAW base units (no decimal conversion — this is the advanced path; prefer wallet_layerx_deposit for LayerX). Returns an action_id; poll wallet_action or pass wait=true. Reuse idempotency_key on retry.', inputSchema: A({ token: S('ERC-20 0x to approve+spend'), amount: S('RAW base-unit approval amount (integer string)'), spender: S('0x approved to pull the amount'), contract: S('0x the call targets'), method: S('Solidity signature, e.g. "depositUSDL(uint256,bytes32)"'), args: { type: 'array' }, idempotency_key: S('optional; reuse across retries'), wait: S('optional "true" to block until terminal') }, ['token', 'amount', 'spender', 'contract', 'method']) },
  // read — poll a durable action (NOT a write; available in reads-only mode).
  { name: 'wallet_action', description: 'Poll a durable action by id: status, phase, approval/call tx hashes, LayerX credit, and — when a step is unhappy — an interpretation-free error envelope (code + retry.strategy + must_not_resubmit + remedy). ok===true means confirmed; ok===false a terminal failure. Pass wait=true to block until the action is terminal. NEVER resubmit a broadcast action — always poll here.', inputSchema: A({ action_id: S('the act_… id returned by wallet_layerx_deposit / wallet_allowance_and_call'), wait: S('optional "true" to block until terminal') }, ['action_id']) },
]

// `tools` is the advertised registry. In reads-only mode the signing/spending
// tools are withheld so the surface is structurally incapable of moving value.
export const tools = READS_ONLY ? ALL_TOOLS.filter((t) => !WRITE_TOOL_NAMES.has(t.name)) : ALL_TOOLS

export const TOOL_NAMES = tools.map((t) => t.name)
export { TOKENS }
