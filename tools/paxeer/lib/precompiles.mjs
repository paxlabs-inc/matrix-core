// Agent-economy precompile encoders (writes) + readers (view/pure calls).
//
// Write builders return {to, data[, value]} ready for wallet.send(). Read
// helpers eth_call the precompile and decode the result. ABIs verified against
// knowledge/HyperPax-OS/precompiles/*/abi.json.
//
// Note: OROB / PoFQ / Clearing / oracle.aggregate are PURE/VIEW pricing +
// reputation math — agents call them read-only to compute oracle-relative
// prices, clearing prices, and fill-quality scores. Actual swaps execute on
// the DEX routers (see config.CONTRACTS.swap), funded through the wallet.

import { PRECOMPILES } from './config.mjs'
import { encodeCall } from './abi.mjs'
import { callMethod } from './rpc.mjs'

export const ADDR = PRECOMPILES

// ── PaymentStreams 0x0906 ──────────────────────────────────────────────────
export const streams = {
  open: ({ payee, token, ratePerSecond, startTime = 0, stopTime = 0, cap }) => ({
    to: PRECOMPILES.streams,
    data: encodeCall('open(address,address,uint256,uint64,uint64,uint256)', [payee, token, ratePerSecond, startTime, stopTime, cap]),
  }),
  settle: (streamId) => ({ to: PRECOMPILES.streams, data: encodeCall('settle(uint256)', [streamId]) }),
  close: (streamId) => ({ to: PRECOMPILES.streams, data: encodeCall('close(uint256)', [streamId]) }),
  updateRate: (streamId, newRate) => ({ to: PRECOMPILES.streams, data: encodeCall('updateRate(uint256,uint256)', [streamId, newRate]) }),
  accrued: (streamId) => callMethod(PRECOMPILES.streams, 'accrued(uint256)', [streamId], ['uint256']),
  getStream: async (streamId) => {
    const t = await callMethod(PRECOMPILES.streams, 'getStream(uint256)', [streamId],
      ['(uint256,address,address,address,uint256,uint256,uint64,uint64,uint256,bool)'])
    if (!t) return null
    const [id, payer, payee, token, ratePerSecond, cap, start, stop, settled, active] = t
    return { id, payer, payee, token, ratePerSecond, cap, startTime: start, stopTime: stop, settled, active }
  },
}

// ── Scheduler 0x0905 ────────────────────────────────────────────────────────
export const scheduler = {
  // payable: `value` (wei) funds the gas deposit for the future execution.
  schedule: ({ target, callData = '0x', executeAtBlock, gasLimit }, value) => ({
    to: PRECOMPILES.scheduler,
    data: encodeCall('schedule(address,bytes,uint64,uint64)', [target, callData, executeAtBlock, gasLimit]),
    value: value !== undefined ? String(value) : undefined,
  }),
  cancel: (jobId) => ({ to: PRECOMPILES.scheduler, data: encodeCall('cancel(uint256)', [jobId]) }),
  reschedule: (jobId, newBlock) => ({ to: PRECOMPILES.scheduler, data: encodeCall('reschedule(uint256,uint64)', [jobId, newBlock]) }),
  getJob: async (jobId) => {
    const t = await callMethod(PRECOMPILES.scheduler, 'getJob(uint256)', [jobId],
      ['(uint256,address,address,bytes,uint64,uint64,uint256,bool)'])
    if (!t) return null
    const [id, creator, target, callData, executeAtBlock, gasLimit, deposit, active] = t
    return { id, creator, target, callData, executeAtBlock, gasLimit, deposit, active }
  },
  pending: (creator) => callMethod(PRECOMPILES.scheduler, 'pending(address)', [creator], ['uint256[]']),
}

// ── Staking 0x0800 (evmos-standard; validatorAddress is bech32 paxvaloper...) ─
export const staking = {
  delegate: ({ delegator, validator, amount }) => ({
    to: PRECOMPILES.staking,
    data: encodeCall('delegate(address,string,uint256)', [delegator, validator, amount]),
  }),
  undelegate: ({ delegator, validator, amount }) => ({
    to: PRECOMPILES.staking,
    data: encodeCall('undelegate(address,string,uint256)', [delegator, validator, amount]),
  }),
  redelegate: ({ delegator, srcValidator, dstValidator, amount }) => ({
    to: PRECOMPILES.staking,
    data: encodeCall('redelegate(address,string,string,uint256)', [delegator, srcValidator, dstValidator, amount]),
  }),
  delegation: async (delegator, validator) => {
    const r = await callMethod(PRECOMPILES.staking, 'delegation(address,string)', [delegator, validator],
      ['uint256', '(string,uint256)'])
    if (!r) return null
    const [shares, coin] = r
    return { shares, balance: { denom: coin[0], amount: coin[1] } }
  },
}

// ── OracleAggregator 0x0903 (price feeds) ──────────────────────────────────
export const oracle = {
  getValidatorPrice: async (marketId) => {
    const r = await callMethod(PRECOMPILES.oracle, 'getValidatorPrice(bytes32)', [marketId],
      ['int256', 'uint256', 'uint256'])
    if (!r) return null
    return { price: r[0], quorum: r[1], timestamp: r[2] }
  },
}

// ── OROB 0x0901 (oracle-relative pricing math, pure) ───────────────────────
export const orob = {
  resolveOffset: (oraclePrice, offsetBps) =>
    callMethod(PRECOMPILES.orob, 'resolveOffset(int256,int16)', [oraclePrice, offsetBps], ['int256']),
  resolveOffsetBatch: (oraclePrice, offsetsBps) =>
    callMethod(PRECOMPILES.orob, 'resolveOffsetBatch(int256,int16[])', [oraclePrice, offsetsBps], ['int256[]']),
  toOffset: (oraclePrice, absolutePrice) =>
    callMethod(PRECOMPILES.orob, 'toOffset(int256,int256)', [oraclePrice, absolutePrice], ['int16']),
}

// ── BatchClearing 0x0902 (uniform-price clearing, pure) ────────────────────
export const clearing = {
  computeClearing: async ({ oraclePrice, buyOffsets, buySizes, sellOffsets, sellSizes }) => {
    const r = await callMethod(PRECOMPILES.clearing,
      'computeClearing(int256,int16[],uint128[],int16[],uint128[])',
      [oraclePrice, buyOffsets, buySizes, sellOffsets, sellSizes],
      ['int16', 'int256', 'uint256'])
    if (!r) return null
    return { clearingOffsetBps: r[0], clearingPrice: r[1], matchedVolume: r[2] }
  },
}

// ── PoFQ 0x0904 (Proof of Fill Quality — reputation math, pure) ────────────
export const pofq = {
  scoreFill: (fillPrice, oraclePrice) =>
    callMethod(PRECOMPILES.pofq, 'scoreFill(int256,int256)', [fillPrice, oraclePrice], ['uint256']),
  scoreBatch: async (fillPrices, oraclePrices, sizes) => {
    const r = await callMethod(PRECOMPILES.pofq, 'scoreBatch(int256[],int256[],uint256[])',
      [fillPrices, oraclePrices, sizes], ['uint256', 'uint256'])
    if (!r) return null
    return { avgScore: r[0], totalVolume: r[1] }
  },
}

// ── ERC-20 write builders ──────────────────────────────────────────────────
export const erc20 = {
  transfer: (token, to, amount) => ({ to: token, data: encodeCall('transfer(address,uint256)', [to, amount]) }),
  approve: (token, spender, amount) => ({ to: token, data: encodeCall('approve(address,uint256)', [spender, amount]) }),
}
