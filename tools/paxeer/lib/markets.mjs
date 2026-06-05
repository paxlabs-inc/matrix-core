// Market + portfolio data layer — the protocol indexers agents read.
//
//   portfolio  -> Argus user-stats indexer (pnl, rank, performance, profile,
//                 charts, trending, dex-history)
//   spot       -> PaxSpot DEX market-data API (generic passthrough)
//   price      -> crossverse price/OHLC API (PAX + bridged majors)
//   points     -> Sidiora points/rewards indexer
//
// Typed helpers cover the confirmed routes; generic `*Get` passthroughs keep
// every documented route reachable without a code change.

import { ENDPOINTS } from './config.mjs'
import { httpGet, qs } from './net.mjs'

// ── Portfolio / Argus user-stats ───────────────────────────────────────────
export const portfolioGet = (path, params) =>
  httpGet(`${ENDPOINTS.portfolio}${path.startsWith('/') ? path : '/' + path}${qs(params)}`)

export const pnl = (addr, days = 30) => portfolioGet(`/api/v1/portfolio/${addr}/pnl`, { days })
export const valueChart = (addr, period = '30d') => portfolioGet(`/api/v1/portfolio/${addr}/charts/value`, { period })
export const pnlChart = (addr, period = '30d') => portfolioGet(`/api/v1/portfolio/${addr}/charts/pnl`, { period })
export const holdingsChart = (addr, period = '30d') => portfolioGet(`/api/v1/portfolio/${addr}/charts/holdings`, { period })
export const rank = (addr) => portfolioGet(`/api/v1/${addr}/rank`)
export const performance = (addr) => portfolioGet(`/api/v1/${addr}/performance`)
export const profile = (addr) => portfolioGet(`/api/v1/${addr}/profile`)
export const dexHistory = (addr, params) => portfolioGet(`/api/v1/${addr}/dex-history`, params)
export const trending = (limit = 20) => portfolioGet(`/api/v1/trending`, { limit })
export const assetChart = (symbol, params) => portfolioGet(`/api/v1/charts/${symbol}`, params)
export const health = () => portfolioGet(`/health`)

// ── PaxSpot DEX market-data (generic passthrough) ──────────────────────────
export const spotGet = (path, params) =>
  httpGet(`${ENDPOINTS.spot}${path.startsWith('/') ? path : '/' + path}${qs(params)}`)

// ── Price + OHLC (crossverse) ──────────────────────────────────────────────
// `cvSymbol` is the crossverse path segment: pax | sol | eth | bnb | sid.
export const price = (cvSymbol = 'pax') =>
  httpGet(`${ENDPOINTS.price}/${String(cvSymbol).toLowerCase()}/price/${qs({ symbol: String(cvSymbol).toUpperCase() })}`)

export const priceGet = (path, params) =>
  httpGet(`${ENDPOINTS.price}${path.startsWith('/') ? path : '/' + path}${qs(params)}`)

// ── Points / rewards ───────────────────────────────────────────────────────
export const pointsBalance = (addr) => httpGet(`${ENDPOINTS.points}/points/balance/${String(addr).toLowerCase()}`)
