// paxeer-net tool registry + dispatch. Reads need no auth; writes route
// through the embedded wallet (network-side custody). A write tool degrades
// gracefully (returns an explanatory result) when no wallet auth is set.

import { CHAIN, ENDPOINTS, CONTRACTS, TOKENS, resolveToken, LIMITS } from './config.mjs'
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
  'stream_open', 'stream_settle', 'stream_close', 'stream_update_rate',
  'schedule_job', 'cancel_job', 'reschedule_job',
  'delegate', 'undelegate', 'redelegate', 'contract_write',
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
      // Pure ABI encode (no network, no send). Used to build callData for
      // schedule_job or to inspect calldata before a contract_write.
      return ok({ tool: name, to: args.to, signature: args.signature, data: encodeCall(args.signature, args.args || []) })
    case 'chain_info': {
      const [bn, cid, sync] = await Promise.all([rpc.blockNumber(), rpc.chainId(), rpc.syncing()])
      return ok({ tool: name, chain: CHAIN, blockNumber: rpc.hexToInt(bn), chainId: rpc.hexToInt(cid), syncing: sync === false ? false : sync, rpc: ENDPOINTS.rpc })
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

    // —— markets / portfolio / price ——
    case 'portfolio': {
      const a = args.address
      const [pnl, rank, perf] = await Promise.allSettled([markets.pnl(a, args.days || 30), markets.rank(a), markets.performance(a)])
      return ok({ tool: name, address: a,
        pnl: pnl.status === 'fulfilled' ? pnl.value : null,
        rank: rank.status === 'fulfilled' ? rank.value : null,
        performance: perf.status === 'fulfilled' ? perf.value : null })
    }
    case 'trending':
      return ok({ tool: name, result: await markets.trending(args.limit || 20) })
    case 'price':
      return ok({ tool: name, result: await markets.price(args.symbol || 'pax') })
    case 'market_get':
      return ok({ tool: name, result: await markets.spotGet(args.path, args.params) })
    case 'points':
      return ok({ tool: name, result: await markets.pointsBalance(await resolveAddr(args.address)) })

    // —— precompile reads (pricing + reputation) ——
    case 'oracle_price':
      return ok({ tool: name, marketId: args.marketId, result: await pc.oracle.getValidatorPrice(args.marketId) })
    case 'orob_resolve':
      return ok({ tool: name, result: await pc.orob.resolveOffset(args.oraclePrice, args.offsetBps) })
    case 'clearing_compute':
      return ok({ tool: name, result: await pc.clearing.computeClearing(args) })
    case 'pofq_score':
      return ok({ tool: name, result: await pc.pofq.scoreFill(args.fillPrice, args.oraclePrice) })
    case 'stream_status':
      return ok({ tool: name, streamId: args.streamId, stream: await pc.streams.getStream(args.streamId), accrued: await pc.streams.accrued(args.streamId) })
    case 'job_status':
      return ok({ tool: name, jobId: args.jobId, job: await pc.scheduler.getJob(args.jobId) })
    case 'jobs_pending':
      return ok({ tool: name, creator: await resolveAddr(args.creator), jobs: await pc.scheduler.pending(await resolveAddr(args.creator)) })
    case 'delegation':
      return ok({ tool: name, result: await pc.staking.delegation(await resolveAddr(args.delegator), args.validator) })

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
      const isNative = !args.token || String(args.token).toUpperCase() === 'PAX'
      if (isNative) {
        const value = toBaseUnits(args.amount, 18)
        return writeTx({ to: args.to, value }, { kind: 'native_transfer', to: args.to, amount: args.amount })
      }
      const tokenAddr = addressFor(args.token)
      if (!tokenAddr) throw new Error(`unknown token ${args.token}`)
      const base = toBaseUnits(args.amount, unitsFor(args.token))
      return writeTx(pc.erc20.transfer(tokenAddr, args.to, base), { kind: 'erc20_transfer', token: tokenAddr, to: args.to, amount: args.amount })
    }
    case 'approve': {
      const tokenAddr = addressFor(args.token)
      if (!tokenAddr) throw new Error(`unknown token ${args.token}`)
      const base = args.amount === 'max' ? ((1n << 256n) - 1n).toString() : toBaseUnits(args.amount, unitsFor(args.token))
      return writeTx(pc.erc20.approve(tokenAddr, args.spender, base), { kind: 'approve', token: tokenAddr, spender: args.spender })
    }

    // —— writes: payment streams 0x0906 ——
    case 'stream_open': {
      const tokenAddr = addressFor(args.token)
      if (!tokenAddr) throw new Error(`stream_open: unknown token ${args.token}`)
      const dec = unitsFor(args.token)
      const ratePerSecond = args.ratePerSecondRaw ?? toBaseUnits(args.ratePerSecond, dec)
      const cap = args.capRaw ?? (args.cap != null ? toBaseUnits(args.cap, dec) : '0')
      const tx = pc.streams.open({ payee: args.payee, token: tokenAddr, ratePerSecond, startTime: args.startTime || 0, stopTime: args.stopTime || 0, cap })
      return writeTx(tx, { kind: 'stream_open', payee: args.payee, token: tokenAddr, ratePerSecond, cap })
    }
    case 'stream_settle':
      return writeTx(pc.streams.settle(args.streamId), { kind: 'stream_settle', streamId: args.streamId })
    case 'stream_close':
      return writeTx(pc.streams.close(args.streamId), { kind: 'stream_close', streamId: args.streamId })
    case 'stream_update_rate': {
      const dec = unitsFor(args.token)
      const newRate = args.newRateRaw ?? toBaseUnits(args.newRate, dec)
      return writeTx(pc.streams.updateRate(args.streamId, newRate), { kind: 'stream_update_rate', streamId: args.streamId, newRate })
    }

    // —— writes: scheduler 0x0905 ——
    case 'schedule_job': {
      const value = args.depositWei ?? (args.deposit != null ? toBaseUnits(args.deposit, 18) : undefined)
      const tx = pc.scheduler.schedule({ target: args.target, callData: args.callData || '0x', executeAtBlock: args.executeAtBlock, gasLimit: args.gasLimit || 200000 }, value)
      return writeTx(tx, { kind: 'schedule_job', target: args.target, executeAtBlock: args.executeAtBlock })
    }
    case 'cancel_job':
      return writeTx(pc.scheduler.cancel(args.jobId), { kind: 'cancel_job', jobId: args.jobId })
    case 'reschedule_job':
      return writeTx(pc.scheduler.reschedule(args.jobId, args.newBlock), { kind: 'reschedule_job', jobId: args.jobId, newBlock: args.newBlock })

    // —— writes: staking 0x0800 ——
    case 'delegate': {
      const delegator = await resolveAddr(args.delegator)
      return writeTx(pc.staking.delegate({ delegator, validator: args.validator, amount: toBaseUnits(args.amount, 18) }), { kind: 'delegate', validator: args.validator, amount: args.amount })
    }
    case 'undelegate': {
      const delegator = await resolveAddr(args.delegator)
      return writeTx(pc.staking.undelegate({ delegator, validator: args.validator, amount: toBaseUnits(args.amount, 18) }), { kind: 'undelegate', validator: args.validator, amount: args.amount })
    }
    case 'redelegate': {
      const delegator = await resolveAddr(args.delegator)
      return writeTx(pc.staking.redelegate({ delegator, srcValidator: args.srcValidator, dstValidator: args.dstValidator, amount: toBaseUnits(args.amount, 18) }), { kind: 'redelegate', src: args.srcValidator, dst: args.dstValidator })
    }

    // —— writes: generic contract call (DEX swaps, any precompile/contract) ——
    case 'contract_write': {
      const data = args.data ?? encodeCall(args.signature, args.args || [])
      const value = args.value != null ? toBaseUnits(args.value, 18) : (args.valueWei ?? undefined)
      return writeTx({ to: args.to, data, value, gas: args.gas }, { kind: 'contract_write', to: args.to, signature: args.signature })
    }

    default:
      throw new Error(`unknown tool: ${name}`)
  }
}

// ── tool descriptors (advertised to the MCP client) ───────────────────────
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })

const ALL_TOOLS = [
  // reads — node
  { name: 'rpc_call', description: 'Direct EVM JSON-RPC call (read-only). args: method, params[].', inputSchema: A({ method: S('JSON-RPC method e.g. eth_getBlockByNumber'), params: { type: 'array' } }, ['method']) },
  { name: 'eth_call', description: 'Read-only eth_call against a contract. Does NOT send a tx.', inputSchema: A({ to: S('contract address'), data: S('0x calldata'), block: S('block tag') }, ['to']) },
  { name: 'contract_read', description: 'Encode+eth_call a method and decode outputs. args: to, signature e.g. "balanceOf(address)", args[], outputs[] e.g. ["uint256"].', inputSchema: A({ to: S('contract'), signature: S('method signature'), args: { type: 'array' }, outputs: { type: 'array' }, block: S('block tag') }, ['to', 'signature']) },
  { name: 'encode_call', description: 'Pure ABI-encode a method call to 0x calldata (no network, no send). Use to build callData for schedule_job, or to inspect calldata. args: signature e.g. "transfer(address,uint256)", args[], to? (echoed back).', inputSchema: A({ signature: S('method signature'), args: { type: 'array' }, to: S('optional contract address to echo') }, ['signature']) },
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
  { name: 'portfolio', description: 'Argus portfolio: pnl + rank + performance for an address.', inputSchema: A({ address: S('0x address'), days: N('pnl window days') }, ['address']) },
  { name: 'trending', description: 'Trending tokens from the discovery indexer.', inputSchema: A({ limit: N('default 20') }) },
  { name: 'price', description: 'Off-chain price for PAX or a bridged major. args: symbol (pax|sol|eth|bnb|sid).', inputSchema: A({ symbol: S('pax|sol|eth|bnb|sid') }) },
  { name: 'market_get', description: 'Generic PaxSpot DEX market-data GET passthrough. args: path, params{}.', inputSchema: A({ path: S('spot api path'), params: { type: 'object' } }, ['path']) },
  { name: 'points', description: 'Sidiora points/rewards balance for an address.', inputSchema: A({ address: S('0x address; optional') }) },
  // reads — precompiles
  { name: 'oracle_price', description: 'OracleAggregator 0x0903 validator price for a market. args: marketId (bytes32).', inputSchema: A({ marketId: S('bytes32 market id') }, ['marketId']) },
  { name: 'orob_resolve', description: 'OROB 0x0901 oracle-relative price: resolveOffset(oraclePrice,int16 offsetBps).', inputSchema: A({ oraclePrice: S('int256'), offsetBps: N('int16 basis points') }, ['oraclePrice', 'offsetBps']) },
  { name: 'clearing_compute', description: 'BatchClearing 0x0902 uniform-price clearing over buy/sell offset+size arrays.', inputSchema: A({ oraclePrice: S('int256'), buyOffsets: { type: 'array' }, buySizes: { type: 'array' }, sellOffsets: { type: 'array' }, sellSizes: { type: 'array' } }, ['oraclePrice']) },
  { name: 'pofq_score', description: 'PoFQ 0x0904 fill-quality score: scoreFill(fillPrice,oraclePrice). Reputation grounded in delivery.', inputSchema: A({ fillPrice: S('int256'), oraclePrice: S('int256') }, ['fillPrice', 'oraclePrice']) },
  { name: 'stream_status', description: 'PaymentStreams 0x0906 stream detail + accrued amount.', inputSchema: A({ streamId: S('uint256 stream id') }, ['streamId']) },
  { name: 'job_status', description: 'Scheduler 0x0905 job detail.', inputSchema: A({ jobId: S('uint256 job id') }, ['jobId']) },
  { name: 'jobs_pending', description: 'Scheduler 0x0905 pending job ids for a creator (defaults to agent wallet).', inputSchema: A({ creator: S('0x address; optional') }) },
  { name: 'delegation', description: 'Staking 0x0800 delegation (shares + balance) for a validator. validator is bech32 paxvaloper...', inputSchema: A({ validator: S('paxvaloper... bech32'), delegator: S('0x address; optional') }, ['validator']) },
  // identity / wallet
  { name: 'wallet_info', description: 'Resolve the agent embedded-wallet address + chain (provisions on first use).', inputSchema: A({}) },
  { name: 'sign_message', description: 'EIP-191 personal_sign a message with the agent wallet (proof of identity).', inputSchema: A({ message: S('message to sign') }, ['message']) },
  // writes — payments
  { name: 'transfer', description: 'Send PAX (token omitted/"PAX") or an ERC-20. args: to, amount (human), token?', inputSchema: A({ to: S('recipient 0x'), amount: S('human amount e.g. "1.5"'), token: S('symbol or 0x; omit for PAX') }, ['to', 'amount']) },
  { name: 'approve', description: 'ERC-20 approve. args: token, spender, amount (human or "max").', inputSchema: A({ token: S('symbol or 0x'), spender: S('0x'), amount: S('human or "max"') }, ['token', 'spender', 'amount']) },
  { name: 'stream_open', description: 'Open a PaymentStream 0x0906. args: payee, token, ratePerSecond (human/sec), cap?, startTime?, stopTime?', inputSchema: A({ payee: S('0x'), token: S('symbol or 0x'), ratePerSecond: S('human per second'), cap: S('human cap'), startTime: N('unix secs, 0=now'), stopTime: N('unix secs, 0=open') }, ['payee', 'token', 'ratePerSecond']) },
  { name: 'stream_settle', description: 'Settle accrued on a stream (pays the payee).', inputSchema: A({ streamId: S('uint256') }, ['streamId']) },
  { name: 'stream_close', description: 'Close a stream and refund the remainder.', inputSchema: A({ streamId: S('uint256') }, ['streamId']) },
  { name: 'stream_update_rate', description: 'Change a stream rate. args: streamId, newRate (human/sec), token (for decimals).', inputSchema: A({ streamId: S('uint256'), newRate: S('human per second'), token: S('symbol or 0x') }, ['streamId', 'newRate']) },
  // writes — scheduler
  { name: 'schedule_job', description: 'Schedule a future tx via Scheduler 0x0905. args: target, callData, executeAtBlock, gasLimit?, deposit? (PAX for gas).', inputSchema: A({ target: S('0x'), callData: S('0x calldata'), executeAtBlock: N('block height'), gasLimit: N('default 200000'), deposit: S('human PAX') }, ['target', 'executeAtBlock']) },
  { name: 'cancel_job', description: 'Cancel a scheduled job and refund the deposit.', inputSchema: A({ jobId: S('uint256') }, ['jobId']) },
  { name: 'reschedule_job', description: 'Move a scheduled job to a new block.', inputSchema: A({ jobId: S('uint256'), newBlock: N('block height') }, ['jobId', 'newBlock']) },
  // writes — staking
  { name: 'delegate', description: 'Stake PAX to a validator (0x0800). args: validator (paxvaloper...), amount (human PAX).', inputSchema: A({ validator: S('paxvaloper...'), amount: S('human PAX'), delegator: S('0x; optional') }, ['validator', 'amount']) },
  { name: 'undelegate', description: 'Unbond PAX from a validator.', inputSchema: A({ validator: S('paxvaloper...'), amount: S('human PAX'), delegator: S('0x; optional') }, ['validator', 'amount']) },
  { name: 'redelegate', description: 'Move stake between validators.', inputSchema: A({ srcValidator: S('paxvaloper...'), dstValidator: S('paxvaloper...'), amount: S('human PAX'), delegator: S('0x; optional') }, ['srcValidator', 'dstValidator', 'amount']) },
  // writes — generic (DEX swaps + any contract/precompile)
  { name: 'contract_write', description: 'Sign+send a contract/precompile write via the wallet. Provide signature+args (encoded for you) OR raw data. args: to, signature?, args[]?, data?, value? (human PAX). Use for DEX swaps on CONTRACTS.swap routers.', inputSchema: A({ to: S('contract/precompile 0x'), signature: S('method signature'), args: { type: 'array' }, data: S('0x calldata (overrides signature)'), value: S('human PAX to attach'), gas: S('gas limit') }, ['to']) },
]

// `tools` is the advertised registry. In reads-only mode the signing/spending
// tools are withheld so the surface is structurally incapable of moving value.
export const tools = READS_ONLY ? ALL_TOOLS.filter((t) => !WRITE_TOOL_NAMES.has(t.name)) : ALL_TOOLS

export const TOOL_NAMES = tools.map((t) => t.name)
export { CONTRACTS, TOKENS }
