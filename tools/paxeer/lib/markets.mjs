// Market price data layer.

import { ENDPOINTS } from './config.mjs'
import { httpGet, qs } from './net.mjs'

// ── Price + OHLC (crossverse) ──────────────────────────────────────────────
// `cvSymbol` is the crossverse path segment: pax | sol | eth | bnb | sid.
export const price = (cvSymbol = 'pax') =>
  httpGet(`${ENDPOINTS.price}/${String(cvSymbol).toLowerCase()}/price/${qs({ symbol: String(cvSymbol).toUpperCase() })}`)

export const priceGet = (path, params) =>
  httpGet(`${ENDPOINTS.price}${path.startsWith('/') ? path : '/' + path}${qs(params)}`)
