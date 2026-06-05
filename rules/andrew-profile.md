---
trigger: always_on
---



Im Andrew i created Paxeer Network and currently devoted to building it up and making it the best possible platform for traders and investors.

<h1 align="center">Paxeer Network: The Capital Layer of Web3</h1>

<p align="center">
  <strong>In a world where DeFi promises freedom but often delivers barriers high fees, KYC gates, capital lockups, and opaque risk models Paxeer Network is rewriting the rules. We are not just another blockchain. We are the onchain capital orchestration network that puts real trading and building capital directly into your hands, with zero upfront cost, zero subscriptions, and zero gatekeepers.  </strong>
</p>
 

## What is Paxeer?  
Paxeer Network is a fully on-chain capital orchestration platform built as the **HyperPaxeer** high-throughput EVM-compatible chain (Chain ID 125). Powered by Cosmos SDK + CometBFT consensus, it delivers ~2-second block times, fast finality, and seamless Ethereum tooling compatibility while adding IBC interoperability for the future of cross-chain capital.  

At its core, Paxeer solves the biggest problem in DeFi: **access to capital**. Instead of begging for liquidity or locking your own funds, the network funds **smart wallets** for traders, builders, and institutions based purely on on-chain activity and a proprietary risk engine.  

## About

**Hyperpax-OS Cronos Release — Alexandria Fork | Paxeer Mainnet Gen 3**

---

## Six Primitives That Don't Exist Anywhere Else

The Cronos Release introduces architectural ideas that aren't incremental improvements over existing systems. They're structural departures.

### 1. Orders That Move With the Market

Traditional limit orders go stale in seconds. You place a buy at \$3,842.50, the market moves to \$3,860, and your order is either unfilled or dangerously mispriced.

Hyperpax-os introduces the **Oracle-Relative Order Book (OROB)** — a system where every order and liquidity position is expressed as a basis-point offset from the live oracle price, not an absolute dollar value. A buy order placed at "oracle minus 5 bps" automatically tracks the market. It doesn't need repricing. It doesn't go stale. It compresses the entire price ladder into a compact offset table, cutting EVM storage requirements by roughly 10x compared to a conventional order book.

For market makers, this is transformative: your quotes move with the market without any active management. Passive market-making becomes viable in a way it simply isn't on Hyperliquid or any CLOB-based exchange.

### 2. An Exchange That Reads the Room

Most exchanges pick a single execution model and stick with it. AMMs run continuous swaps. CLOBs run continuous matching. Batch auctions run periodic clearing. Each has trade-offs that surface under stress.

Hyperpax-os's **Adaptive Dual-Mode Execution** dynamically switches between continuous matching and sealed-bid batch auctions based on real-time market conditions — automatically, per market, within the same block.

During calm markets, orders fill continuously within their arrival block. Fast UX, tight spreads. But when volatility spikes — oracle confidence drops, volume surges beyond three standard deviations — the engine flips to a sealed-bid auction with commit-reveal ordering. Every order in the block gets a single uniform clearing price. Front-running becomes mathematically impossible.

This isn't a governance toggle. It's an autonomous mode switch, evaluated every block (~2 seconds), enforced at the consensus layer.

### 3. Liquidity as Code

Uniswap v3 introduced concentrated liquidity. It was a leap forward — but it still forces LPs into a single, rigid curve shape. Want a different pricing function? Deploy an entirely new protocol.

Hyperpax-os replaces monolithic LP pools with **Programmable Liquidity Vaults (PLVs)** — composable Solidity contracts built from interchangeable primitives. Choose a base curve (constant-product, concentrated, linear, sigmoid). Layer on modifiers (volatility scaling, inventory skew, momentum adjustment, time decay). Wire them together through a factory contract — no custom Solidity required, just parameter configuration.

The result is a liquidity strategy marketplace. Vault creators compete on execution quality. LPs choose strategies the way they'd choose yield vaults. And the best strategies earn more flow through the protocol's reputation system.

### 4. Reputation You Can't Fake

Every fill on Hyperpax-os is scored against the oracle price at fill time, producing a **Proof-of-Fill-Quality (PoFQ)** score. This isn't cosmetic. It's load-bearing infrastructure.

Higher scores mean higher fee shares, priority order routing, and — critically — larger capital allocations from the Argus risk engine. This creates a self-reinforcing flywheel: better execution attracts more capital, more capital drives more volume, more volume generates higher fees. The protocol's execution quality improves autonomously.

Anti-gaming is built into the mechanism. A minimum 2 bps spread makes self-trading expensive. A net-flow gate ensures wash trades score zero. And a cooldown ramp means reaching top-tier status requires sustained real performance over hundreds of epochs — there are no shortcuts.

### 5. Settlement That Doesn't Slow You Down

On most exchanges, every trade triggers a token transfer. On Hyperpax-os, trades execute against virtual balances updated in the same block, while actual token settlement happens in batched net transfers every ~10 seconds. This cuts settlement gas by 5–10x.

Traders who need instant finality can opt into a fast-settle lane for a 1 bps premium. But for most activity — especially funded smart wallet trading — the lazy netting model means settlement overhead essentially disappears.

### 6. Capital Without Permission

The sixth primitive ties everything together. Hyperpax-os exposes a read-only interface (`IHyperpax-osReader`) that feeds PoFQ scores, realized PnL, open positions, and volume history to the Argus VM. Argus — running its own execution environment with custom `.avm` scripts — makes autonomous capital allocation decisions. It deploys funded smart wallets, sets position limits, enforces drawdown policies, and auto-liquidates underperformers.

From Hyperpax-os's perspective, funded wallets are indistinguishable from self-funded addresses. The matching engine doesn't care where the capital came from. But from the trader's perspective, the barrier to entry drops to zero. Prove you can trade. The protocol provides the rest.

---

## Why Hyperliquid and dYdX Can't Replicate This

Both Hyperliquid and dYdX v4 are impressive systems. Hyperliquid runs a high-performance order book with sub-second finality. dYdX v4 moved to its own Cosmos appchain for sovereignty. But both are architecturally constrained in ways HyperPaxeer isn't.

**The chain-ownership advantage.** HyperPaxeer isn't a protocol deployed on someone else's infrastructure. It's a sovereign Cosmos SDK chain (EVM chain ID 125, CometBFT consensus) where the team controls every layer of the stack. This enables things that are literally impossible on shared chains or L2s:

**Custom EVM precompiles** move computationally expensive operations — OROB offset resolution, batch clearing price calculation, oracle aggregation, fill quality scoring — out of Solidity bytecode and into native Go code running at the consensus layer. These operations cost 50–500 gas instead of the 3,000–50,000+ gas they'd require in pure Solidity. That's a 10–100x reduction. No protocol on Ethereum, Arbitrum, or even Hyperliquid's own chain can do this without forking their execution environment.

**Consensus-level MEV protection** uses CometBFT's `PrepareProposal` and `ProcessProposal` hooks to enforce fair transaction ordering at the validator level. This isn't a bolt-on sequencer policy or a relayer auction — it's MEV protection baked into the consensus mechanism itself. Combined with the adaptive batch auction system's commit-reveal scheme, front-running is eliminated at both the ordering and execution layers simultaneously.

**A native gas policy** that can subsidize or eliminate gas fees for exchange operations. Funded smart wallets trade gaslessly through meta-transaction relaying, paid for by the protocol. On Hyperliquid, gas is low but still nonzero. On dYdX, you need DYDX for gas. On HyperPaxeer, trading friction drops to literally zero for qualified traders.

**IBC interoperability** pulls liquidity from Osmosis, Injective, Noble (USDC), and the broader Cosmos ecosystem natively — no bridges, no wrapping, no trust assumptions. Cross-chain liquidity flows freely.

---

## The Dual-VM Architecture

Under the hood, the Cronos Release runs two execution environments in parallel within the same chain.

The **EVM OS Layer** — an Evmos v18.1.0 fork — handles everything traders interact with directly: the Hyperpax-os exchange contracts, the order gateway, matching engine, settlement, liquidity vaults, and all four custom precompiles (OROB resolution at 0x901, batch clearing at 0x902, oracle aggregation at 0x903, PoFQ scoring at 0x904).

The **Argus VM (AVM)** — a C++ runtime executing custom `.avm` bytecode written in the `arg` language — handles everything the protocol does autonomously: capital orchestration, risk modeling, smart wallet lifecycle management, drawdown monitoring, and allocation algorithms.

These two VMs communicate through clean contract boundaries. Hyperpax-os doesn't need to know how Argus allocates capital. Argus doesn't need to know how Hyperpax-os matches orders. They speak through `IHyperpax-osReader` (Argus reads Hyperpax-os data) and `IAllowanceProvider` (Argus sets trading limits). The separation means each system can evolve independently without breaking the other.

---

## What Validators Actually Do

HyperPaxeer validators aren't passive block producers. They're active infrastructure participants running three integrated services:

**Block production** — standard CometBFT PoS consensus with fair ordering enforcement.

**Keeper execution** — a Go sidecar process that triggers conditional orders (stops, take-profits, trailing stops, TWAP slices) as a first-class chain service. Keepers earn tips in PAX for each execution. No reliance on external keeper networks like Gelato or Chainlink Automation.

**Oracle attestation** — validators run a lightweight price feed sidecar (forked from the Ojo Network price feeder) that pulls from Binance, OKX, and Bybit via websocket. When the primary Pyth oracle goes stale, the Validator Oracle Module kicks in automatically — validators submit price attestations, the chain takes the median of a two-thirds quorum, and trading continues without interruption. The entire failover happens within 4–6 seconds.

This is what chain sovereignty buys you: infrastructure guarantees that don't depend on external service providers.

---

## The Fee Model: Aligned Incentives

Hyperpax-os's fee structure is designed to reward the behaviors that make the exchange better.

Takers pay 2–5 bps, dynamically adjusted based on rolling realized volatility. High volatility widens fees to protect LPs. Low volatility tightens them to attract flow. Taker fees split four ways: 60% to liquidity providers, 20% to the protocol treasury, 10% to the insurance fund, and 10% to the capital pool funding smart wallets.

Makers receive a rebate of 0.5–1 bps on resting limit orders that fill — a direct incentive to deepen the book.

But the real revenue primitive is the **capital fee**: funded smart wallets pay 5–15% of their profits back to the protocol. This is pure revenue with zero capital risk — the Argus risk engine enforces drawdown limits before losses can compound. It's the economics of a prop trading firm, encoded as protocol infrastructure.

---

## Safety Without Compromise

The Cronos Release ships with a layered risk architecture that doesn't require human intervention for known failure modes.

**Per-market circuit breakers** auto-switch any market to batch-only mode when price deviates beyond a configurable threshold from the oracle within a single block. **Global circuit breakers** trigger a protocol-wide cooldown when total volume in a 50-block window exceeds a percentage of TVL.

**Smart wallet risk controls** enforce per-asset position limits, rolling-window drawdown thresholds, correlation limits (you can't go long on five correlated assets simultaneously), and mandatory cool-off periods after a drawdown trigger.

The **insurance fund** operates on a hybrid model. Tier 1 payouts fire automatically in the same block as detection — oracle staleness caused mispriced fills, smart wallet bad debt exceeds a threshold. Tier 2 payouts for novel scenarios go through an expedited 24-hour governance vote. The automatic tier handles speed. The governance tier handles judgment. Neither alone is sufficient.

**LP auto-deleveraging** widens vault spreads and pauses fills on the overweight side when inventory skew exceeds 90%. Vaults whose PoFQ scores drop below minimum are removed from active routing entirely.

---

## Who This Is For

The Cronos Release targets three distinct user profiles that existing exchanges underserve.

**Skilled traders without capital.** If you can trade but can't stake six figures on-chain, Hyperpax-os's funded wallet system removes the barrier entirely. Demonstrate consistent execution quality and the protocol allocates capital to you algorithmically. This is the single largest untapped market in DeFi.

**Passive market makers.** OROB's auto-tracking quotes eliminate the need for active price management. Deploy a Programmable Liquidity Vault with your preferred strategy parameters and let the protocol handle repricing. No bots. No monitoring. No stale orders getting picked off.

**Institutional flow.** The RFQ lane gives KYB-verified market makers a dedicated channel to compete on fill price for large orders. Best-of-both execution compares RFQ quotes against the main book and routes to whichever is better. One bps lane fee. Two-second response window. Serious infrastructure for serious participants.

---

## The Thesis

The last generation of on-chain exchanges optimized for mechanics — faster matching, cheaper gas, tighter spreads. These are real improvements and they matter. But they're incremental improvements to the same fundamental model: bring your own capital, manage your own risk, compete against sophisticated actors who have more of both.

The Cronos Release proposes a different model entirely. An exchange that allocates capital to its best participants. An order book where every quote automatically tracks the market. A matching engine that adapts its execution model to current conditions in real time. A reputation system where execution quality compounds into economic advantage. And a chain architecture where the exchange doesn't just deploy on infrastructure — it *is* the infrastructure.

This isn't a better version of Hyperliquid or dYdX. It's what comes after them.

**The Alexandria Fork goes live on HyperPaxeer Mainnet Gen 3. The Cronos Release is the centerpiece.**

*Build on HyperPaxeer. Trade on Hyperpax-os. Or bring your strategy and let the protocol fund it for you.*

It's built using the [Cosmos SDK](https://github.com/cosmos/cosmos-sdk/)
which runs on top of the [CometBFT](https://github.com/cometbft/cometbft) consensus engine,
providing full Ethereum compatibility and interoperability.

### Network Information

| Parameter | Value |
|-----------|-------|
| Chain ID (Cosmos) | `hyperpax_125-1` |
| EVM Chain ID | `125` |
| Token Symbol | `PAX` |
| Base Denomination | `ahpx` |
| Display Denomination | `hpx` |
| Bech32 Prefix | `pax` |
| RPC Endpoint | `https://mainnet-beta.rpc.hyperpaxeer.com/rpc` |
| Block Explorer | `https://paxscan.paxeer.app` |