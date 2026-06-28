// kindle tool registry + dispatch.
//
// READS (Neo-callable, no signing): pools, market detail, quotes, pending fees,
// optical info, positions. WRITES (escalate-class, reachable only via MCL's
// core_execute): launch, buy, sell, swap, collect fees, set fee strategy, and
// optical creation. On Neo's read-only surface (KINDLE_READS_ONLY=1) the write
// tools are withheld AND refused at dispatch — the surface is structurally
// incapable of moving value (spec req.1/req.3).

import {
  CONTRACTS, ENDPOINTS, USDL, TOKENS, resolveToken,
  resolvePresetOptical, PRESET_OPTICALS, strategyToEnum, FEE_STRATEGY_NAME, LIMITS,
} from './config.mjs'
import {
  callMethod, decimalsFor, tokenAddress, allowance, erc20Balance, approveCalldata,
  encodeCall, decode, toBaseUnits, fromBaseUnits, sendTx, agentAddress, walletConfigured,
  explorerTx, rpc, MAX_APPROVAL,
} from './chain.mjs'
import { keccak256Utf8 } from '../../paxeer/lib/keccak.mjs'
import { buildCustomOptical } from './optical_template.mjs'

const ZERO = '0x0000000000000000000000000000000000000000'

// ── result helper ─────────────────────────────────────────────────────────────
function ok(obj) {
  return { content: [{ type: 'text', text: typeof obj === 'string' ? obj : JSON.stringify(obj) }] }
}

// READS-ONLY surface: the lane Neo's conversational agent gets. Withholds every
// signing/spending tool so value-moving actions can only go through core_execute.
const READS_ONLY = process.env.KINDLE_READS_ONLY === '1'
const WRITE_TOOL_NAMES = new Set([
  'kindle_launch', 'kindle_buy', 'kindle_sell', 'kindle_swap',
  'kindle_collect_fees', 'kindle_set_fee_strategy',
  'kindle_create_optical_preset', 'kindle_create_optical_custom',
])

const decline = (reason, extra = {}) => ok({ ok: false, declined: true, reason, ...extra })

// ownerFor — the acting wallet address. Prefers an explicit `from` (lets MCL
// simulate / dry-run for a specific wallet) and otherwise resolves the agent
// embedded wallet (provisioning on first use).
async function ownerFor(args) {
  if (args && /^0x[0-9a-fA-F]{40}$/.test(String(args.from))) return String(args.from)
  return agentAddress()
}

function deadlineFrom(arg) {
  if (arg !== undefined && arg !== null && String(arg) !== '') {
    const n = Number(arg)
    if (!Number.isFinite(n) || n <= 0) throw new Error(`invalid deadline ${arg}`)
    // Treat small values as a relative horizon in seconds; large as absolute unix.
    return n < 10_000_000 ? Math.floor(Date.now() / 1000) + n : Math.floor(n)
  }
  return Math.floor(Date.now() / 1000) + LIMITS.deadlineSeconds
}

function slippageBpsFrom(arg) {
  if (arg === undefined || arg === null || String(arg) === '') return LIMITS.defaultSlippageBps
  const n = Number(arg)
  if (!Number.isInteger(n) || n < 0 || n >= 10000) throw new Error(`invalid slippageBps ${arg} (0..9999)`)
  return n
}

// minOut = quoted * (10000 - slippageBps) / 10000, all integer (req.11 ac_3).
function applySlippage(quotedBase, slippageBps) {
  const q = BigInt(quotedBase)
  return (q * BigInt(10000 - slippageBps)) / 10000n
}

// ── read decoders ─────────────────────────────────────────────────────────────
// Pool metadata is sourced from the pool contract's own views (token/optical/
// timestamp are precompile-free and reliable) plus the NFT id + owner. We avoid
// the registry's aggregate metadata view, which can be unavailable on some of
// the load-balanced RPC nodes.
async function readPoolMetadata(pool) {
  const info = await callMethod(
    pool, 'getPoolInfo()', [],
    ['(address,address,uint256,uint256,uint256,uint256,uint256)'],
  ).catch(() => null)
  if (!info) return null
  const [token, optical, , , , createdAt] = info
  if (!token || token === ZERO) return null
  const nftId = await callMethod(CONTRACTS.poolRegistry, 'getNftIdByPool(address)', [pool], ['uint256']).catch(() => null)
  const creator = nftId != null
    ? await callMethod(CONTRACTS.sidioraNft, 'ownerOf(uint256)', [nftId], ['address']).catch(() => null)
    : null
  return { creator, token, optical, nftId, createdAt }
}

async function readPoolStats(pool) {
  const s = await callMethod(
    CONTRACTS.quoter, 'getPoolStats(address)', [pool],
    ['(uint256,uint256,uint256,uint256,uint256,uint256,uint256,uint256)'],
  ).catch(() => null)
  if (!s) return null
  const [virtualUsdl, realUsdl, tokenReserve, cumulativeVolume, currentFeeBps, poolAge, marketCap, price] = s
  return { virtualUsdl, realUsdl, tokenReserve, cumulativeVolume, currentFeeBps, poolAge, marketCap, price }
}

// usdl helper: integer base (6-dec) -> human string.
const usdlHuman = (base) => fromBaseUnits(base ?? '0', USDL.decimals)

// ── the embedded-wallet write executor ────────────────────────────────────────
// Executes a plan of {label, to, data, value?} calls sequentially through the
// single signing path. On a 403 leash denial it stops and surfaces a plain
// structured denial (never retried as transient — req.2 ac_3 / req.6 ac_5).
async function execPlan(plan, extra = {}) {
  if (!walletConfigured()) {
    return ok({
      ok: false, error: 'wallet not configured',
      hint: 'the embedded-wallet agent lane is unset (executor key / WALLET_API); value-moving ops run inside MCL where it is provisioned',
      planned: plan.map((p) => ({ label: p.label, to: p.to, data: p.data, value: p.value })),
      ...extra,
    })
  }
  const sent = []
  for (const call of plan) {
    try {
      const r = await sendTx({ to: call.to, data: call.data, value: call.value })
      sent.push({ label: call.label, tx_hash: r.tx_hash, explorer: r.explorer })
    } catch (e) {
      const denial = mapDenial(e)
      if (denial) return ok({ ok: false, declined: true, ...denial, completed: sent, ...extra })
      throw e
    }
  }
  return ok({ ok: true, txs: sent, ...extra })
}

// mapDenial converts a wallet 403 (frozen / wrong mode / over cap) into a
// plain-language structured denial. Returns null for non-leash errors.
function mapDenial(e) {
  if (!e || e.status !== 403) return null
  const code = (e.body && (e.body.error || e.body.code)) || 'DENIED'
  const reasons = {
    FROZEN: 'your wallet is frozen, so this was not done',
    WALLET_FROZEN: 'your wallet is frozen, so this was not done',
    READ_ONLY: 'your wallet is set to read-only, so trading/launching is not allowed',
    MODE_NOT_ALLOWED: 'your wallet mode does not allow this action',
    CAP_EXCEEDED: 'this would exceed your spending limit, so it was not done',
    SPEND_CAP_EXCEEDED: 'this would exceed your spending limit, so it was not done',
  }
  return { code, reason: reasons[code] || `your wallet declined this action (${code})` }
}

// ensureApproval — returns an approve call when current allowance < needed.
async function ensureApproval(token, spender, needed, owner) {
  if (!token || token === ZERO) return null
  const have = await allowance(token, owner, spender)
  if (have >= BigInt(needed)) return null
  return { label: `approve ${token}`, to: token, data: approveCalldata(spender, MAX_APPROVAL) }
}

// parse the MarketCreated(token,pool,creator,nftId) log from a launch receipt.
const MARKET_CREATED_TOPIC = keccak256Utf8('MarketCreated(address,address,address,uint256)')
async function parseLaunchReceipt(txHash) {
  const receipt = await rpc('eth_getTransactionReceipt', [txHash]).catch(() => null)
  if (!receipt || !Array.isArray(receipt.logs)) return null
  for (const log of receipt.logs) {
    if ((log.topics || [])[0]?.toLowerCase() === MARKET_CREATED_TOPIC.toLowerCase()) {
      const token = '0x' + log.topics[1].slice(26)
      const pool = '0x' + log.topics[2].slice(26)
      const creator = '0x' + log.topics[3].slice(26)
      const nftId = BigInt(log.data || '0x0').toString()
      return { token, pool, creator, nftId }
    }
  }
  return null
}

// ── dispatch ────────────────────────────────────────────────────────────────
export async function dispatch(name, args = {}) {
  if (READS_ONLY && WRITE_TOOL_NAMES.has(name)) {
    return ok({
      ok: false, tool: name,
      error: 'read-only kindle surface: launch/trade/fee/optical actions are disabled here',
      hint: 'value-moving KindleLaunch actions run through the secure execution path (core_execute), not this read-only bridge',
    })
  }
  const dryRun = args.dry_run === true || args.dry_run === 'true'

  switch (name) {
    // ───────────────────────── READS ─────────────────────────
    case 'kindle_pools': {
      const offset = Number(args.offset ?? 0)
      const limit = Math.min(Number(args.limit ?? 20), 100)
      const count = await callMethod(CONTRACTS.poolRegistry, 'getPoolCount()', [], ['uint256'])
      const pools = await callMethod(CONTRACTS.quoter, 'getAllPools(uint256,uint256)', [offset, limit], ['address[]'])
      return ok({ ok: true, total: count, offset, limit, pools: pools || [] })
    }

    case 'kindle_market': {
      const pool = await resolvePool(args.pool || args.token)
      if (!pool) return ok({ ok: false, miss: true, reason: 'pool not found for the given pool/token' })
      const [meta, stats] = await Promise.all([readPoolMetadata(pool), readPoolStats(pool)])
      if (!meta || !meta.token) return ok({ ok: false, miss: true, pool, reason: 'pool not registered' })
      let tokenDecimals = 18
      try { tokenDecimals = await decimalsFor(meta.token) } catch { /* keep default */ }
      return ok({
        ok: true, pool, token: meta.token, optical: meta.optical, nftId: meta.nftId,
        creator: meta.creator, createdAt: meta.createdAt,
        stats: stats && {
          priceUsdl: fromBaseUnits(stats.price, 18), marketCapUsdl: usdlHuman(stats.marketCap),
          realUsdl: usdlHuman(stats.realUsdl), virtualUsdl: usdlHuman(stats.virtualUsdl),
          tokenReserve: fromBaseUnits(stats.tokenReserve, tokenDecimals),
          cumulativeVolumeUsdl: usdlHuman(stats.cumulativeVolume),
          currentFeeBps: stats.currentFeeBps, poolAgeSeconds: stats.poolAge, raw: stats,
        },
        explorer: `${ENDPOINTS.explorer}/address/${pool}`,
      })
    }

    case 'kindle_quote': {
      const pool = await resolvePool(args.pool || args.token)
      if (!pool) return ok({ ok: false, miss: true, reason: 'pool not found' })
      const side = String(args.side || 'buy').toLowerCase()
      const isBuy = side === 'buy'
      const meta = await readPoolMetadata(pool)
      if (!meta || !meta.token) return ok({ ok: false, miss: true, pool, reason: 'pool not registered' })
      const inDecimals = isBuy ? USDL.decimals : await decimalsFor(meta.token)
      const outDecimals = isBuy ? await decimalsFor(meta.token) : USDL.decimals
      const amountIn = toBaseUnits(args.amount, inDecimals)
      const q = await callMethod(
        CONTRACTS.quoter, 'quoteExactInput(address,uint256,bool)', [pool, amountIn, isBuy],
        ['(uint256,uint256,uint256)'],
      )
      if (!q) return ok({ ok: false, miss: true, pool, reason: 'quote unavailable' })
      const [amountOut, feeAmount, priceImpactBps] = q
      const slippageBps = slippageBpsFrom(args.slippageBps)
      const minOut = applySlippage(amountOut, slippageBps).toString()
      return ok({
        ok: true, pool, side, token: meta.token,
        amountIn, amountInHuman: fromBaseUnits(amountIn, inDecimals),
        amountOut, amountOutHuman: fromBaseUnits(amountOut, outDecimals),
        feeAmount: isBuy ? usdlHuman(feeAmount) : fromBaseUnits(feeAmount, inDecimals),
        priceImpactBps, slippageBps,
        minOut, minOutHuman: fromBaseUnits(minOut, outDecimals),
      })
    }

    case 'kindle_fees': {
      const pool = await resolvePool(args.pool || args.token)
      if (!pool) return ok({ ok: false, miss: true, reason: 'pool not found' })
      const [accrued, airdrop, lp, surplus] = await Promise.all([
        callMethod(CONTRACTS.feeAccumulator, 'getAccumulatedFees(address)', [pool], ['uint256']).catch(() => '0'),
        callMethod(CONTRACTS.feeAccumulator, 'getAirdropBalance(address)', [pool], ['uint256']).catch(() => '0'),
        callMethod(CONTRACTS.feeAccumulator, 'getLpRewardsBalance(address)', [pool], ['uint256']).catch(() => '0'),
        callMethod(CONTRACTS.feeAccumulator, 'getOpticalSurplus(address)', [pool], ['uint256']).catch(() => '0'),
      ])
      return ok({
        ok: true, pool,
        accumulatedFeesUsdl: usdlHuman(accrued), airdropBalanceUsdl: usdlHuman(airdrop),
        lpRewardsBalanceUsdl: usdlHuman(lp), opticalSurplusUsdl: usdlHuman(surplus),
        raw: { accrued, airdrop, lp, surplus },
      })
    }

    case 'kindle_optical_info': {
      const ref = args.optical
      const preset = resolvePresetOptical(ref)
      const addr = preset ? preset.address : (/^0x[0-9a-fA-F]{40}$/.test(String(ref)) ? String(ref) : null)
      if (!addr) {
        return ok({ ok: true, presets: Object.values(PRESET_OPTICALS).map((p) => ({ key: p.key, name: p.name, address: p.address, description: p.description })) })
      }
      const [registered, md] = await Promise.all([
        callMethod(CONTRACTS.opticalRegistry, 'isRegistered(address)', [addr], ['bool']),
        callMethod(CONTRACTS.opticalRegistry, 'getOpticalMetadata(address)', [addr], ['(string,string,uint8,string,uint256)']).catch(() => null),
      ])
      const meta = md && { name: md[0], description: md[1], riskLevel: md[2], auditor: md[3], registeredAt: md[4] }
      return ok({
        ok: true, optical: addr, registered: registered === true,
        preset: preset ? { key: preset.key, name: preset.name, description: preset.description } : null,
        metadata: meta,
        note: registered === true ? 'verified on the registry' : 'works fully whether or not it shows as verified (registration is an admin-only trust signal)',
      })
    }

    case 'kindle_position': {
      const owner = (/^0x[0-9a-fA-F]{40}$/.test(String(args.address)) && args.address) || await agentAddress()
      if (!owner) return ok({ ok: false, miss: true, reason: 'no wallet address' })
      let token = tokenAddress(args.token)
      let pool = null
      if (!token && args.pool) {
        pool = await resolvePool(args.pool)
        const meta = pool && await readPoolMetadata(pool)
        token = meta && meta.token
      }
      if (!token) return ok({ ok: false, miss: true, reason: 'provide token or pool' })
      const [bal, dec] = await Promise.all([erc20Balance(token, owner), decimalsFor(token).catch(() => 18)])
      return ok({ ok: true, owner, token, pool, balance: bal.toString(), balanceHuman: fromBaseUnits(bal.toString(), dec) })
    }

    // ───────────────────────── WRITES ─────────────────────────
    case 'kindle_launch': {
      const strategy = strategyToEnum(args.feeStrategy ?? 'CLAIM')
      if (strategy === null) return decline(`unknown fee strategy "${args.feeStrategy}" — choose CLAIM, BURN, AIRDROP, or LP_REWARDS`)
      if (!args.name || !args.symbol) return decline('a token name and symbol are required to launch')
      const optical = await resolveOpticalAddress(args.optical)
      if (optical === undefined) return decline(`unknown optical "${args.optical}" — use a preset name, a 0x address, or omit for none`)
      const owner = await ownerFor(args)
      const creationFee = BigInt(await callMethod(CONTRACTS.protocolConfig, 'creationFee()', [], ['uint256']) ?? '0')
      const plan = []
      if (creationFee > 0n) {
        const appr = await ensureApproval(USDL.address, CONTRACTS.router, creationFee, owner)
        if (appr) plan.push(appr)
      }
      const data = encodeCall('createMarket(string,string,uint8,address)', [args.name, args.symbol, strategy, optical])
      plan.push({ label: 'createMarket', to: CONTRACTS.router, data })
      const extra = {
        action: 'launch', name: args.name, symbol: args.symbol,
        feeStrategy: FEE_STRATEGY_NAME[strategy], optical,
        creationFeeUsdl: usdlHuman(creationFee.toString()),
      }
      if (dryRun) return ok({ ok: true, dry_run: true, planned: plan, ...extra })
      const res = await execPlan(plan, extra)
      const parsed = JSON.parse(res.content[0].text)
      if (parsed.ok && parsed.txs && parsed.txs.length) {
        const launchTx = parsed.txs[parsed.txs.length - 1].tx_hash
        const outcome = await parseLaunchReceipt(launchTx)
        if (outcome) return ok({ ...parsed, ...outcome, explorer: explorerTx(launchTx) })
      }
      return res
    }

    case 'kindle_buy': {
      const pool = await resolvePool(args.pool || args.token)
      if (!pool) return decline('pool not found for that token')
      const owner = await ownerFor(args)
      const usdlIn = toBaseUnits(args.amount, USDL.decimals)
      const q = await callMethod(CONTRACTS.quoter, 'quoteExactInput(address,uint256,bool)', [pool, usdlIn, true], ['(uint256,uint256,uint256)']).catch(() => null)
      if (!q) return decline('could not get a quote for this buy')
      const slippageBps = slippageBpsFrom(args.slippageBps)
      const minOut = applySlippage(q[0], slippageBps).toString()
      const deadline = deadlineFrom(args.deadline)
      const plan = []
      const appr = await ensureApproval(USDL.address, CONTRACTS.router, usdlIn, owner)
      if (appr) plan.push(appr)
      plan.push({ label: 'buy', to: CONTRACTS.router, data: encodeCall('buy(address,uint256,uint256,uint256)', [pool, usdlIn, minOut, deadline]) })
      const extra = { action: 'buy', pool, usdlIn, usdlInHuman: fromBaseUnits(usdlIn, USDL.decimals), minTokensOut: minOut, slippageBps, deadline }
      return dryRun ? ok({ ok: true, dry_run: true, planned: plan, ...extra }) : execPlan(plan, extra)
    }

    case 'kindle_sell': {
      const pool = await resolvePool(args.pool || args.token)
      if (!pool) return decline('pool not found for that token')
      const meta = await readPoolMetadata(pool)
      if (!meta || !meta.token) return decline('pool not registered')
      const owner = await ownerFor(args)
      const tokenDecimals = await decimalsFor(meta.token)
      const tokenIn = toBaseUnits(args.amount, tokenDecimals)
      const q = await callMethod(CONTRACTS.quoter, 'quoteExactInput(address,uint256,bool)', [pool, tokenIn, false], ['(uint256,uint256,uint256)']).catch(() => null)
      if (!q) return decline('could not get a quote for this sell')
      const slippageBps = slippageBpsFrom(args.slippageBps)
      const minOut = applySlippage(q[0], slippageBps).toString()
      const deadline = deadlineFrom(args.deadline)
      const plan = []
      const appr = await ensureApproval(meta.token, CONTRACTS.router, tokenIn, owner)
      if (appr) plan.push(appr)
      plan.push({ label: 'sell', to: CONTRACTS.router, data: encodeCall('sell(address,uint256,uint256,uint256)', [pool, tokenIn, minOut, deadline]) })
      const extra = { action: 'sell', pool, token: meta.token, tokenIn, tokenInHuman: fromBaseUnits(tokenIn, tokenDecimals), minUsdlOut: minOut, minUsdlOutHuman: usdlHuman(minOut), slippageBps, deadline }
      return dryRun ? ok({ ok: true, dry_run: true, planned: plan, ...extra }) : execPlan(plan, extra)
    }

    case 'kindle_swap': {
      const tokenIn = tokenAddress(args.tokenIn)
      const tokenOut = tokenAddress(args.tokenOut)
      if (!tokenIn || !tokenOut) return decline('both tokenIn and tokenOut (symbol or 0x) are required')
      if (tokenIn.toLowerCase() === tokenOut.toLowerCase()) return decline('tokenIn and tokenOut must differ')
      const owner = await ownerFor(args)
      const inDecimals = await decimalsFor(tokenIn)
      const outDecimals = await decimalsFor(tokenOut)
      const amountIn = toBaseUnits(args.amount, inDecimals)
      const q = await callMethod(
        CONTRACTS.quoter, 'quoteMultihop(address,address,uint256)', [tokenIn, tokenOut, amountIn],
        ['(uint256,uint256,uint256,uint256,uint256,uint256,uint256,address,address)'],
      ).catch(() => null)
      if (!q) return decline('could not get a route/quote for this swap (no pool/route for that pair)')
      const slippageBps = slippageBpsFrom(args.slippageBps)
      const minOut = applySlippage(q[0], slippageBps).toString()
      const deadline = deadlineFrom(args.deadline)
      const plan = []
      const appr = await ensureApproval(tokenIn, CONTRACTS.router, amountIn, owner)
      if (appr) plan.push(appr)
      plan.push({ label: 'swap', to: CONTRACTS.router, data: encodeCall('swapTokenForToken(address,address,uint256,uint256,uint256)', [tokenIn, tokenOut, amountIn, minOut, deadline]) })
      const extra = { action: 'swap', tokenIn, tokenOut, amountIn, amountInHuman: fromBaseUnits(amountIn, inDecimals), minAmountOut: minOut, minAmountOutHuman: fromBaseUnits(minOut, outDecimals), slippageBps, deadline }
      return dryRun ? ok({ ok: true, dry_run: true, planned: plan, ...extra }) : execPlan(plan, extra)
    }

    case 'kindle_collect_fees': {
      const resolved = await resolveNftAndPool(args)
      if (resolved.error) return decline(resolved.error)
      const { nftId, pool } = resolved
      const owner = await ownerFor(args)
      const nftOwner = await callMethod(CONTRACTS.sidioraNft, 'ownerOf(uint256)', [nftId], ['address']).catch(() => null)
      if (!nftOwner || nftOwner.toLowerCase() !== String(owner).toLowerCase()) {
        return decline('you do not own this pool, so its fees cannot be collected', { nftId, nftOwner })
      }
      const strategy = Number(await callMethod(CONTRACTS.sidioraNft, 'getFeeStrategy(uint256)', [nftId], ['uint8']))
      const accrued = BigInt(await callMethod(CONTRACTS.feeAccumulator, 'getAccumulatedFees(address)', [pool], ['uint256']) ?? '0')
      if (accrued === 0n && strategy !== 2) return decline('there are no fees to collect yet', { nftId, pool })
      const fn = ['claimFees', 'executeBurn', 'executeAirdrop', 'executeLpRewards'][strategy]
      if (!fn) return decline('this pool has an unrecognized fee strategy', { nftId, strategy })
      const data = encodeCall(`${fn}(uint256)`, [nftId])
      const extra = { action: 'collect_fees', nftId, pool, strategy: FEE_STRATEGY_NAME[strategy], accumulatedFeesUsdl: usdlHuman(accrued.toString()) }
      const plan = [{ label: fn, to: CONTRACTS.feesRouter, data }]
      return dryRun ? ok({ ok: true, dry_run: true, planned: plan, ...extra }) : execPlan(plan, extra)
    }

    case 'kindle_set_fee_strategy': {
      const resolved = await resolveNftAndPool(args)
      if (resolved.error) return decline(resolved.error)
      const { nftId } = resolved
      const strategy = strategyToEnum(args.strategy)
      if (strategy === null) return decline(`unknown fee strategy "${args.strategy}" — choose CLAIM, BURN, AIRDROP, or LP_REWARDS`)
      const owner = await ownerFor(args)
      const nftOwner = await callMethod(CONTRACTS.sidioraNft, 'ownerOf(uint256)', [nftId], ['address']).catch(() => null)
      if (!nftOwner || nftOwner.toLowerCase() !== String(owner).toLowerCase()) {
        return decline('you do not own this pool, so its fee strategy cannot be changed', { nftId, nftOwner })
      }
      const data = encodeCall('setFeeStrategy(uint256,uint8)', [nftId, strategy])
      const extra = { action: 'set_fee_strategy', nftId, strategy: FEE_STRATEGY_NAME[strategy] }
      const plan = [{ label: 'setFeeStrategy', to: CONTRACTS.feesRouter, data }]
      return dryRun ? ok({ ok: true, dry_run: true, planned: plan, ...extra }) : execPlan(plan, extra)
    }

    case 'kindle_create_optical_preset': {
      const preset = resolvePresetOptical(args.preset)
      if (preset) {
        return ok({
          ok: true, mode: 'attach-singleton', preset: preset.key, name: preset.name,
          optical: preset.address, description: preset.description,
          note: 'attach this address as the optical when launching (kindle_launch optical=' + preset.address + '); no deploy needed',
        })
      }
      // Configured per-creator instance requires the LaunchpadOpticalFactory.
      const factory = CONTRACTS.launchpadOpticalFactory
      if (!factory || factory === ZERO) {
        return decline(
          'a self-service optical factory is not deployed yet, so a custom-configured preset cannot be created on demand',
          {
            available_presets: Object.values(PRESET_OPTICALS).map((p) => ({ key: p.key, name: p.name, description: p.description })),
            alternative: 'attach one of the deployed presets above, or build a bespoke one with kindle_create_optical_custom',
          },
        )
      }
      // Factory deployed: create a configured LaunchpadOptical instance.
      const teamWallets = Array.isArray(args.teamWallets) ? args.teamWallets : []
      const cliff = Number(args.cliffDuration ?? 0)
      const vesting = Number(args.vestingDuration ?? 0)
      const capitalRaiseBps = Number(args.capitalRaiseBps ?? 0)
      const capitalRaiseDuration = Number(args.capitalRaiseDuration ?? 0)
      const teamClaimAddress = args.teamClaimAddress || await agentAddress()
      if (capitalRaiseBps > 1000) return decline('capitalRaiseBps cannot exceed 1000 (10%)')
      const data = encodeCall(
        'createLaunchpadOptical(address[],uint256,uint256,uint256,uint256,address)',
        [teamWallets, cliff, vesting, capitalRaiseBps, capitalRaiseDuration, teamClaimAddress],
      )
      const extra = { action: 'create_optical_preset', mode: 'factory-deploy', factory, capitalRaiseBps, cliff, vesting }
      const plan = [{ label: 'createLaunchpadOptical', to: factory, data }]
      return dryRun ? ok({ ok: true, dry_run: true, planned: plan, ...extra }) : execPlan(plan, extra)
    }

    case 'kindle_create_optical_custom': {
      let built
      try {
        built = buildCustomOptical({
          name: args.name, hooks: args.hooks, bodies: args.bodies,
          immutables: args.immutables, notes: args.notes,
        })
      } catch (e) {
        return decline(String(e.message || e))
      }
      // The bridge AUTHORS the contract; compile/test/deploy run through the
      // Tachyon tools (a separate MCP surface), driven by the planner. We return
      // the source + the exact next steps so MCL can carry it the rest of the way.
      return ok({
        ok: true, action: 'create_optical_custom',
        contractName: built.contractName, flags: built.flags, flagsBinary: built.flagsBinary,
        hooks: built.hooks, solcVersion: built.solcVersion, source: built.source,
        constructorArgs: built.ctorArgs.map((a) => ({ ...a,
          value: a.name === 'poolRegistry' ? CONTRACTS.poolRegistry : (a.name === 'owner' ? 'YOUR_EMBEDDED_WALLET' : undefined) })),
        next_steps: [
          'tachyon_compile the source',
          'tachyon_test it (deploy only on green)',
          `tachyon_deploy on chain ${CONTRACTS.router ? 125 : 125} with constructor [poolRegistry=${CONTRACTS.poolRegistry}, owner=<embedded wallet>] and an idempotency_key`,
          'attach the deployed address as the optical when launching (kindle_launch optical=<address>)',
        ],
        note: 'a freshly deployed optical is fully functional immediately; it will show as "unverified" until an admin registers it on the OpticalRegistry — that never blocks use',
      })
    }

    default:
      throw new Error(`unknown tool: ${name}`)
  }
}

// ── resolution helpers ────────────────────────────────────────────────────────
// resolvePool — accept a pool address directly, or a token address -> its pool.
async function resolvePool(ref) {
  if (!ref) return null
  const s = String(ref).trim()
  if (!/^0x[0-9a-fA-F]{40}$/.test(s)) {
    const t = resolveToken(s)
    if (t && t.address) return resolvePool(t.address)
    return null
  }
  // Detect a pool directly via its own view (precompile-free): a real pool
  // returns a non-zero tokenAddress(); a plain token reverts.
  const asPoolToken = await callMethod(s, 'tokenAddress()', [], ['address']).catch(() => null)
  if (asPoolToken && asPoolToken !== ZERO) return s
  // Otherwise treat it as a token and look up its pool.
  const pool = await callMethod(CONTRACTS.poolRegistry, 'getPoolByToken(address)', [s], ['address']).catch(() => null)
  return pool && pool !== ZERO ? pool : null
}

// resolveOpticalAddress — preset name|address|none -> 0x. undefined = unknown ref.
async function resolveOpticalAddress(ref) {
  if (ref === undefined || ref === null || ref === '' || String(ref).toLowerCase() === 'none') return ZERO
  const preset = resolvePresetOptical(ref)
  if (preset) return preset.address
  if (/^0x[0-9a-fA-F]{40}$/.test(String(ref))) return String(ref)
  return undefined
}

// resolveNftAndPool — accept {nftId} or {pool}; fill in the missing one.
async function resolveNftAndPool(args) {
  let nftId = args.nftId
  let pool = null
  if (args.pool) {
    pool = await resolvePool(args.pool)
    if (!pool) return { error: 'pool not found' }
    if (nftId === undefined || nftId === null || nftId === '') {
      nftId = await callMethod(CONTRACTS.poolRegistry, 'getNftIdByPool(address)', [pool], ['uint256']).catch(() => null)
    }
  }
  if (nftId === undefined || nftId === null || nftId === '') return { error: 'provide an nftId or a pool' }
  if (!pool) {
    pool = await callMethod(CONTRACTS.sidioraNft, 'getPoolAddress(uint256)', [nftId], ['address']).catch(() => null)
  }
  return { nftId: String(nftId), pool }
}

// ── advertised tool descriptors ───────────────────────────────────────────────
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })
const B = (description) => ({ type: 'boolean', description })

const ALL_TOOLS = [
  // reads
  { name: 'kindle_pools', description: 'List KindleLaunch pools (paginated discovery). args: offset?, limit? (<=100).', inputSchema: A({ offset: N('start index'), limit: N('page size, default 20') }) },
  { name: 'kindle_market', description: 'Token/market detail: price, market cap, reserves, fee, optical, creator. args: pool (or token).', inputSchema: A({ pool: S('pool 0x'), token: S('token 0x or symbol') }) },
  { name: 'kindle_quote', description: 'Quote a trade and the quote-bounded minimum out. args: pool (or token), amount (human), side (buy|sell), slippageBps?.', inputSchema: A({ pool: S('pool 0x'), token: S('token 0x/symbol'), amount: S('human amount'), side: S('buy|sell'), slippageBps: N('default 100 = 1%') }, ['amount']) },
  { name: 'kindle_fees', description: 'Pending/accumulated fees for a pool in USDL (+ airdrop/LP/optical balances). args: pool (or token).', inputSchema: A({ pool: S('pool 0x'), token: S('token 0x/symbol') }) },
  { name: 'kindle_optical_info', description: 'Optical detail: name/description/risk + whether it is verified on the registry. args: optical (preset name or 0x). Omit to list presets.', inputSchema: A({ optical: S('preset name or 0x address') }) },
  { name: 'kindle_position', description: 'A wallet\'s token balance for a pool/token. args: token (or pool), address? (defaults to the agent wallet).', inputSchema: A({ token: S('token 0x/symbol'), pool: S('pool 0x'), address: S('holder 0x; optional') }) },
  // writes (escalate-class)
  { name: 'kindle_launch', description: 'Launch a token + market. args: name, symbol, feeStrategy (CLAIM|BURN|AIRDROP|LP_REWARDS), optical? (preset name|0x|none), dry_run?.', inputSchema: A({ name: S('token name'), symbol: S('token symbol'), feeStrategy: S('CLAIM|BURN|AIRDROP|LP_REWARDS'), optical: S('preset name, 0x, or none'), dry_run: B('plan only, do not send') }, ['name', 'symbol']) },
  { name: 'kindle_buy', description: 'Buy a token with USDL (quote-bounded slippage + deadline). args: pool (or token), amount (human USDL), slippageBps?, deadline?, dry_run?.', inputSchema: A({ pool: S('pool 0x'), token: S('token 0x/symbol'), amount: S('human USDL'), slippageBps: N('default 100'), deadline: N('secs from now or unix'), dry_run: B('plan only') }, ['amount']) },
  { name: 'kindle_sell', description: 'Sell a token for USDL (quote-bounded slippage + deadline). args: pool (or token), amount (human tokens), slippageBps?, deadline?, dry_run?.', inputSchema: A({ pool: S('pool 0x'), token: S('token 0x/symbol'), amount: S('human token amount'), slippageBps: N('default 100'), deadline: N('secs from now or unix'), dry_run: B('plan only') }, ['amount']) },
  { name: 'kindle_swap', description: 'Swap token A -> USDL -> token B in one tx (end-to-end slippage). args: tokenIn, tokenOut, amount (human), slippageBps?, deadline?, dry_run?.', inputSchema: A({ tokenIn: S('0x/symbol'), tokenOut: S('0x/symbol'), amount: S('human amount of tokenIn'), slippageBps: N('default 100'), deadline: N('secs from now or unix'), dry_run: B('plan only') }, ['tokenIn', 'tokenOut', 'amount']) },
  { name: 'kindle_collect_fees', description: 'Collect a pool\'s fees per its current strategy (NFT-owner gated). args: nftId (or pool), dry_run?.', inputSchema: A({ nftId: S('NFT id'), pool: S('pool 0x'), dry_run: B('plan only') }) },
  { name: 'kindle_set_fee_strategy', description: 'Change a pool\'s fee strategy (NFT-owner gated). args: nftId (or pool), strategy (CLAIM|BURN|AIRDROP|LP_REWARDS), dry_run?.', inputSchema: A({ nftId: S('NFT id'), pool: S('pool 0x'), strategy: S('CLAIM|BURN|AIRDROP|LP_REWARDS'), dry_run: B('plan only') }, ['strategy']) },
  { name: 'kindle_create_optical_preset', description: 'Attach a deployed preset optical, or (if a factory is deployed) deploy a configured LaunchpadOptical. args: preset (antisnipe|maxwallet|tax|cooldown|buybackburn) OR launchpad config (teamWallets[], cliffDuration, vestingDuration, capitalRaiseBps<=1000, capitalRaiseDuration, teamClaimAddress), dry_run?.', inputSchema: A({ preset: S('preset key/name'), teamWallets: { type: 'array' }, cliffDuration: N('secs'), vestingDuration: N('secs'), capitalRaiseBps: N('<=1000'), capitalRaiseDuration: N('secs'), teamClaimAddress: S('0x'), dry_run: B('plan only') }) },
  { name: 'kindle_create_optical_custom', description: 'Author a BaseOptical-derived custom optical (returns compilable Solidity + getFlags bitmap + the Tachyon deploy steps). args: name, hooks[] (beforeSwap|afterSwap|beforeFeeDistribution|afterFeeDistribution), bodies{hook:solidity}?, immutables[{type,name}]?, notes?.', inputSchema: A({ name: S('contract name'), hooks: { type: 'array' }, bodies: { type: 'object' }, immutables: { type: 'array' }, notes: S('what it should do') }, ['hooks']) },
]

// In reads-only mode the signing/spending tools are withheld so the surface is
// structurally incapable of moving value.
export const tools = READS_ONLY ? ALL_TOOLS.filter((t) => !WRITE_TOOL_NAMES.has(t.name)) : ALL_TOOLS
export const TOOL_NAMES = tools.map((t) => t.name)
export { ALL_TOOLS, WRITE_TOOL_NAMES }
