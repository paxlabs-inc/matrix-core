---
layout: layouts/blog-post.njk
title: "The Paxeer Network: Purpose-Built for Agents"
date: 2025-01-05
author: "Infrastructure Team"
tags: ["infrastructure", "blockchain"]
excerpt: "Why we built a purpose-built L1 with 400ms finality for agentic workloads — and what it means for the machine economy."
---

Matrix doesn't run on Ethereum or Solana. It runs on Paxeer — a purpose-built Layer 1 blockchain designed specifically for agentic workloads.

## Why a New Chain?

Existing L1s optimize for human transaction patterns: variable gas, long finality windows, and synchronous execution. Agents need something different: predictable costs, instant finality, and high-throughput event streams.

## Key Specs

- **Block time**: 400ms
- **Finality**: 400ms (single-slot)
- **Consensus**: HyperPaxeer 125
- **Settlement**: EIP-712 receipts with PAX credit ledger

## Agent-Native Design

Every aspect of Paxeer is optimized for machine-to-machine interaction:

- Metered invocation with sub-cent precision
- Receipt-based settlement (no gas bidding)
- Native DID resolution for agent identity
- Event streams optimized for SSE consumption

This is the substrate that makes Matrix's deterministic execution economically viable at scale.
