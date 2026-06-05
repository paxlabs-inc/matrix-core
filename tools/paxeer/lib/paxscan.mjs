// PaxScan — Paxeer's Blockscout v2 explorer API. Read-only chain data:
// addresses, transactions, tokens, holders, logs, NFTs, search, stats.
// Base hits `${paxscan}/api/v2{path}`. A generic `get()` passthrough keeps
// any documented Blockscout v2 route reachable without a code change.

import { ENDPOINTS } from './config.mjs'
import { httpGet, qs } from './net.mjs'

const base = () => `${ENDPOINTS.paxscan}/api/v2`

export const get = (path, params) => httpGet(`${base()}${path.startsWith('/') ? path : '/' + path}${qs(params)}`)

// Addresses
export const address = (h) => get(`/addresses/${h}`)
export const addressCounters = (h) => get(`/addresses/${h}/counters`)
export const addressTransactions = (h, params) => get(`/addresses/${h}/transactions`, params)
export const addressTokenTransfers = (h, params) => get(`/addresses/${h}/token-transfers`, params)
export const addressTokens = (h, params) => get(`/addresses/${h}/tokens`, params)
export const addressTokenBalances = (h) => get(`/addresses/${h}/token-balances`)
export const addressLogs = (h, params) => get(`/addresses/${h}/logs`, params)
export const addressNfts = (h, params) => get(`/addresses/${h}/nft`, params)
export const addressCoinHistory = (h, params) => get(`/addresses/${h}/coin-balance-history`, params)

// Tokens
export const tokens = (params) => get(`/tokens`, params)
export const token = (h) => get(`/tokens/${h}`)
export const tokenHolders = (h, params) => get(`/tokens/${h}/holders`, params)
export const tokenTransfers = (h, params) => get(`/tokens/${h}/transfers`, params)

// Transactions
export const transaction = (h) => get(`/transactions/${h}`)
export const txTokenTransfers = (h, params) => get(`/transactions/${h}/token-transfers`, params)
export const txLogs = (h, params) => get(`/transactions/${h}/logs`, params)

// Network-wide
export const search = (q) => get(`/search`, { q })
export const stats = () => get(`/stats`)
export const mainPageTransactions = () => get(`/main-page/transactions`)
export const mainPageBlocks = () => get(`/main-page/blocks`)
