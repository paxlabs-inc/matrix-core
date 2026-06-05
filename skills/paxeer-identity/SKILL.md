---
name: paxeer-identity
description: Establish and prove the agent's Paxeer network identity — resolve/provision the embedded wallet (network-side custody), bind its address to a durable cortex Identity, report the on-chain footprint, and sign messages as proof. Read + sign only; never moves value.
origin: Matrix/Paxeer
---

# Paxeer Identity

L1 of the Paxeer machine-commerce stack: **agent identity**. Before an agent
can pay, trade, stake, or schedule on Paxeer, it needs a wallet and a durable
sense of *who it is* on the network. This skill resolves that identity and
writes it into cortex so every other `paxeer-*` skill can act under it.

## Custody model (why this is safe)

The agent's wallet is an **embedded wallet** at `connect.paxportwallet.com`.
Keys live with the network custody service and signing happens server-side —
the bridge never sees key material. Because custody is network-side, spending
limits, allow-lists, and policy are enforced *on the wallet at the network
layer*, not trusted to agent code. That is precisely why this is the right
identity surface for autonomous agents acting on behalf of users.

## Tool mandate (the planner MUST follow)

1. ALWAYS resolve the address with a `paxeer-net.wallet_info` tool_call. It
   provisions the wallet on first use and returns the authoritative `address`
   + `chainId`. NEVER invent or guess an address.
2. To establish identity *on behalf of an owner*, persist a cortex `Identity`
   memory binding the owner's name/DID to the resolved address
   (`Wallets:[address]`) + chain. This binding is what lets `paxeer-pay`,
   `paxeer-trade`, etc. act under the right identity.
3. For network standing, add read-only `paxeer-net.get_balance` and
   `address_overview` (PaxScan footprint: tx count, token holdings).
4. To prove identity, use `paxeer-net.sign_message` (EIP-191). Use this only
   when a signature/attestation is explicitly requested.

## Hard guardrails

- This skill is **read + sign only**. It never transfers, swaps, stakes, or
  streams value. Route any value movement to `paxeer-pay` / `paxeer-trade` /
  `paxeer-stake` / `paxeer-schedule`, which enforce the spend policy.
- Do not overwrite an existing `Identity` binding with a different address
  without explicit instruction (`bind_conflict` is a policy failure).

## Reporting

Finish with one report spoken to Andrew in natural, conversational prose:
the wallet address verbatim from `wallet_info`, the chain, native PAX balance,
a one-line on-chain footprint, and what (if anything) was bound or signed.
Ground every claim in the tool results — never from memory. Persist the run as
an `Event` so the chronology stays accurate.
