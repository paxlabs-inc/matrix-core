#!/usr/bin/env node
// finance — MCP stdio bridge giving Matrix agents real market data.
//
// It reaches market data ONLY through the matrix-router's finance lane, never
// the vendors directly. That is deliberate: the router holds the Financial
// Modeling Prep and Alpha Vantage keys, caches by freshness class, collapses
// concurrent identical requests, rate-limits per user and counts every upstream
// call. Going through it means the agent asking for a quote and the user opening
// that symbol in /finance share ONE cache entry, ONE vendor quota and ONE bill —
// and no vendor key is ever copied into a per-user machine.
//
// Wiring:
//   ROUTER_INTERNAL_URL    — the router's internal base URL
//   ROUTER_FINANCE_TOKEN  — the service token for the finance lane
// MATRIX_FINANCE_URL and MATRIX_FINANCE_TOKEN remain accepted for compatibility
// with machines provisioned by older routers.
// Optional: MATRIX_FINANCE_TIMEOUT_MS (default 20000).
//
// No API key required to BOOT: the server always starts and advertises its
// tools (so executor/mcp Manager.verifyTools passes); missing wiring degrades to
// a structured "not configured" result only at call time.
//
// Results are shaped for a language model, not for a chart: compact, unit
// labelled, with the as-of stamp and the answering provider named, and an
// explicit line when data is stale, partial or unavailable.
//
// Wire protocol mirrors tools/websearch/web-search.mjs (newline-delimited
// JSON-RPC over stdio, zero npm deps, Node 18+ global fetch).
// Run `node tools/finance/finance.mjs --selftest` to smoke it offline.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const SERVER_NAME = 'finance'
const SERVER_VERSION = '0.1.0'

const TIMEOUT_MS = clampInt(process.env.MATRIX_FINANCE_TIMEOUT_MS, 20000, 1000, 120000)
const MAX_RESPONSE_BYTES = clampInt(process.env.MATRIX_FINANCE_MAX_RESPONSE_BYTES, 8_000_000, 10_000, 40_000_000)

// A model gets a readable series, not four hundred bars of JSON.
const SERIES_POINTS = 40

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

function laneURL() {
  const explicit = (process.env.MATRIX_FINANCE_URL || '').trim().replace(/\/+$/, '')
  if (explicit) return explicit
  const router = (process.env.ROUTER_INTERNAL_URL || 'http://matrix-router.railway.internal:8088')
    .trim()
    .replace(/\/+$/, '')
  return router ? `${router}/internal/finance` : ''
}
function laneToken() {
  return (process.env.MATRIX_FINANCE_TOKEN || process.env.ROUTER_FINANCE_TOKEN || '').trim()
}
function configured() {
  return laneURL() !== '' && laneToken() !== ''
}

function ok(obj) {
  return { content: [{ type: 'text', text: typeof obj === 'string' ? obj : JSON.stringify(obj) }] }
}
function fail(tool, error, extra = {}) {
  return {
    content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }],
    isError: true,
  }
}
function notConfigured(tool) {
  return fail(tool, 'market data is not configured', {
    hint: 'the internal market-data service token is unavailable to the finance broker',
  })
}

// ── the lane ─────────────────────────────────────────────────────────────────
async function lane(path, params = {}, options = {}) {
  if (typeof fetch !== 'function') throw new Error('finance: global fetch unavailable (Node 18+ required)')
  const query = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue
    query.set(k, Array.isArray(v) ? v.join(',') : String(v))
  }
  const method = options.method || 'GET'
  const url = `${laneURL()}/${path}${method === 'GET' && query.toString() ? `?${query}` : ''}`
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  const headers = { Accept: 'application/json', Authorization: `Bearer ${laneToken()}` }
  let body
  if (method !== 'GET') {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(options.body ?? params)
  }
  // The acting user, so the router's metering and rate limiting stay per user
  // even when the call arrives from the agent rather than the browser.
  const subject = (process.env.MATRIX_USER_ID || process.env.MATRIX_ACTOR_DID || '').trim()
  if (subject) headers['X-Matrix-Subject'] = subject

  let res
  try {
    res = await fetch(url, { method, headers, body, signal: controller.signal })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${TIMEOUT_MS}ms` : (e && e.message) || String(e)
    throw new Error(`market data unreachable: ${reason}`)
  }
  clearTimeout(timer)

  let raw = await res.text()
  if (raw.length > MAX_RESPONSE_BYTES) raw = raw.slice(0, MAX_RESPONSE_BYTES)
  let parsed = null
  try {
    parsed = raw ? JSON.parse(raw) : null
  } catch {
    /* non-JSON */
  }
  if (!res.ok) {
    const err = parsed && parsed.error ? parsed.error : null
    const message = err && err.message ? err.message : `market data request failed (HTTP ${res.status})`
    const e = new Error(message)
    e.kind = err && err.kind ? err.kind : 'upstream'
    e.retryAfter = err && err.retry_after_seconds ? err.retry_after_seconds : undefined
    throw e
  }
  return parsed
}

// ── shaping ──────────────────────────────────────────────────────────────────
function stamp(source) {
  if (!source) return {}
  const out = { source: source.provider }
  if (source.fetched_at) out.as_of = source.fetched_at
  if (source.stale) out.note = 'served from the last good value — the provider is not answering right now'
  if (source.fallback) out.provider_note = 'answered by the backup provider'
  return out
}

function num(v, digits = 2) {
  if (v === undefined || v === null || !Number.isFinite(v)) return undefined
  return Number(v.toFixed(digits))
}

function shapeQuote(q) {
  if (!q) return null
  const out = { symbol: q.symbol }
  if (q.name) out.name = q.name
  if (q.exchange) out.exchange = q.exchange
  if (q.class) out.asset_class = q.class
  if (num(q.price) !== undefined) out.price = num(q.price)
  if (num(q.change) !== undefined) out.change = num(q.change)
  if (num(q.change_percent) !== undefined) out.change_percent = num(q.change_percent)
  if (num(q.open) !== undefined) out.open = num(q.open)
  if (num(q.day_low) !== undefined && num(q.day_high) !== undefined) {
    out.day_range = `${num(q.day_low)}–${num(q.day_high)}`
  }
  if (num(q.year_low) !== undefined && num(q.year_high) !== undefined) {
    out.year_range = `${num(q.year_low)}–${num(q.year_high)}`
  }
  if (num(q.previous_close) !== undefined) out.previous_close = num(q.previous_close)
  if (q.volume !== undefined && q.volume !== null) out.volume = q.volume
  if (num(q.market_cap, 0) !== undefined) out.market_cap = num(q.market_cap, 0)
  if (q.as_of) out.quote_time = q.as_of
  if (q.extended) {
    const ext = {}
    if (num(q.extended.price) !== undefined) ext.price = num(q.extended.price)
    if (num(q.extended.bid_price) !== undefined) ext.bid = num(q.extended.bid_price)
    if (num(q.extended.ask_price) !== undefined) ext.ask = num(q.extended.ask_price)
    if (q.extended.as_of) ext.as_of = q.extended.as_of
    if (Object.keys(ext).length) out.extended_hours = ext
  }
  return { ...out, ...stamp(q.source) }
}

// A model reads a shape, not four hundred bars: the endpoints, the extremes, the
// direction, and a downsampled path it can reason over.
function shapeSeries(s) {
  if (!s || !Array.isArray(s.candles) || s.candles.length === 0) return null
  const candles = s.candles
  const first = candles[0]
  const last = candles[candles.length - 1]
  let high = first.h
  let low = first.l
  let volume = 0
  let hasVolume = false
  for (const c of candles) {
    if (c.h > high) high = c.h
    if (c.l < low) low = c.l
    if (typeof c.v === 'number') {
      volume += c.v
      hasVolume = true
    }
  }
  const change = last.c - first.o
  const changePercent = first.o !== 0 ? (change / first.o) * 100 : undefined

  const step = Math.max(1, Math.ceil(candles.length / SERIES_POINTS))
  const points = []
  for (let i = 0; i < candles.length; i += step) {
    points.push({ t: candles[i].t, c: num(candles[i].c, 4) })
  }
  const lastPoint = points[points.length - 1]
  if (!lastPoint || lastPoint.t !== last.t) points.push({ t: last.t, c: num(last.c, 4) })

  const out = {
    symbol: s.symbol,
    interval: s.interval,
    bars: candles.length,
    from: first.t,
    to: last.t,
    open: num(first.o, 4),
    close: num(last.c, 4),
    high: num(high, 4),
    low: num(low, 4),
    change: num(change, 4),
    change_percent: num(changePercent),
    points,
    points_note: `${points.length} sampled closes across ${candles.length} bars — full history is on the /finance chart`,
  }
  if (hasVolume) out.total_volume = volume
  return { ...out, ...stamp(s.source) }
}

function shapeNews(feed, limit) {
  if (!feed || !Array.isArray(feed.items)) return null
  const items = feed.items.slice(0, limit || 20).map((n) => {
    const out = { title: n.title, url: n.url }
    if (n.publisher) out.publisher = n.publisher
    if (n.published_at) out.published = n.published_at
    if (n.symbols && n.symbols.length) out.symbols = n.symbols
    if (n.summary) out.summary = n.summary.length > 400 ? `${n.summary.slice(0, 400)}…` : n.summary
    if (n.sentiment) out.sentiment = `${n.sentiment.label || 'scored'} (${num(n.sentiment.score, 3)})`
    return out
  })
  return { count: items.length, items, ...stamp(feed.source) }
}

function shapeMovers(list) {
  if (!list || !Array.isArray(list.movers)) return null
  return {
    kind: list.kind,
    count: list.movers.length,
    movers: list.movers.slice(0, 25).map((m) => {
      const out = { symbol: m.symbol }
      if (m.name) out.name = m.name
      if (num(m.price) !== undefined) out.price = num(m.price)
      if (num(m.change_percent) !== undefined) out.change_percent = num(m.change_percent)
      if (m.volume !== undefined && m.volume !== null) out.volume = m.volume
      return out
    }),
    ...stamp(list.source),
  }
}

function shapeProfile(p) {
  if (!p) return null
  const out = { symbol: p.symbol }
  for (const [key, value] of Object.entries({
    name: p.name,
    exchange: p.exchange_name || p.exchange,
    sector: p.sector,
    industry: p.industry,
    country: p.country,
    ceo: p.ceo,
    employees: p.employees,
    ipo_date: p.ipo_date,
    website: p.website,
    currency: p.currency,
  })) {
    if (value !== undefined && value !== null && value !== '') out[key] = value
  }
  if (num(p.market_cap, 0) !== undefined) out.market_cap = num(p.market_cap, 0)
  if (num(p.beta) !== undefined) out.beta = num(p.beta)
  if (p.description) {
    out.description = p.description.length > 700 ? `${p.description.slice(0, 700)}…` : p.description
  }
  return { ...out, ...stamp(p.source) }
}

function shapeFundamentals(f) {
  if (!f) return null
  const out = { symbol: f.symbol }
  const fields = {
    market_cap: [f.market_cap, 0],
    pe_ratio: [f.pe_ratio, 2],
    price_to_book: [f.price_to_book, 2],
    price_to_sales: [f.price_to_sales, 2],
    dividend_yield_percent: [f.dividend_yield !== undefined && f.dividend_yield !== null ? f.dividend_yield * 100 : undefined, 3],
    eps: [f.eps, 2],
    return_on_equity_percent: [f.return_on_equity !== undefined && f.return_on_equity !== null ? f.return_on_equity * 100 : undefined, 2],
    net_profit_margin_percent: [f.net_profit_margin !== undefined && f.net_profit_margin !== null ? f.net_profit_margin * 100 : undefined, 2],
    debt_to_equity: [f.debt_to_equity, 2],
    current_ratio: [f.current_ratio, 2],
    enterprise_value: [f.enterprise_value, 0],
  }
  for (const [key, [value, digits]] of Object.entries(fields)) {
    const v = num(value, digits)
    if (v !== undefined) out[key] = v
  }
  if (f.analysts) {
    const a = {}
    for (const key of ['strong_buy', 'buy', 'hold', 'sell', 'strong_sell']) {
      if (f.analysts[key] !== undefined && f.analysts[key] !== null) a[key] = f.analysts[key]
    }
    if (f.analysts.consensus) a.consensus = f.analysts.consensus
    if (num(f.analysts.target_mean) !== undefined) a.price_target_mean = num(f.analysts.target_mean)
    if (num(f.analysts.target_high) !== undefined) a.price_target_high = num(f.analysts.target_high)
    if (num(f.analysts.target_low) !== undefined) a.price_target_low = num(f.analysts.target_low)
    if (Object.keys(a).length) out.analysts = a
  }
  return { ...out, ...stamp(f.source) }
}

function shapeResearch(envelope) {
  const run = envelope?.run || {}
  const output = run.output || undefined
  const shaped = {
    id: run.id,
    status: run.status,
    workflow: envelope?.workflow,
    subject: envelope?.subject,
    cache_hit: envelope?.meta?.cache_hit === true,
    retrieved_at: envelope?.meta?.retrieved_at,
  }
  if (output) {
    shaped.output = {
      text: output.text,
      structured: output.structured,
      grounding: output.grounding || [],
      synthesis_note: 'generated synthesis — verify claims against the attached grounding',
    }
  }
  if (run.costDollars?.total !== undefined) shaped.cost_dollars = run.costDollars.total
  if (run.error) shaped.error = run.error
  return shaped
}

// ── dispatch ─────────────────────────────────────────────────────────────────
const VALID_RANGES = new Set(['1D', '5D', '1M', '6M', 'YTD', '1Y', '5Y', 'MAX'])

function symbolArg(args, name = 'symbol') {
  const v = (args?.[name] ?? '').toString().trim().toUpperCase()
  if (!v) throw new Error(`${name} is required`)
  return v
}

function symbolsArg(args) {
  const raw = args?.symbols
  const list = Array.isArray(raw) ? raw : String(raw ?? '').split(',')
  const out = list.map((s) => String(s).trim().toUpperCase()).filter(Boolean)
  if (out.length === 0) throw new Error('symbols is required')
  return out
}

export async function dispatch(name, args = {}) {
  if (!configured()) return notConfigured(name)
  try {
    switch (name) {
      case 'market_quote': {
        const data = await lane('quote', { symbol: symbolArg(args) })
        return ok({ tool: name, quote: shapeQuote(data) })
      }
      case 'market_quotes': {
        const data = await lane('quotes', { symbols: symbolsArg(args) })
        return ok({ tool: name, quotes: (data?.quotes || []).map(shapeQuote), ...stamp(data?.source) })
      }
      case 'market_series': {
        const range = String(args?.range ?? '1M').toUpperCase()
        if (!VALID_RANGES.has(range)) throw new Error(`range must be one of ${[...VALID_RANGES].join(', ')}`)
        const data = await lane('series', { symbol: symbolArg(args), range })
        const shaped = shapeSeries(data)
        if (!shaped) return fail(name, 'no price history was returned for that symbol')
        return ok({ tool: name, range, series: shaped })
      }
      case 'market_search': {
        const query = (args?.query ?? '').toString().trim()
        if (!query) throw new Error('query is required')
        const data = await lane('search', { q: query, limit: args?.limit })
        return ok({ tool: name, query, matches: data?.matches || [], ...stamp(data?.source) })
      }
      case 'market_profile': {
        const data = await lane('profile', { symbol: symbolArg(args) })
        return ok({ tool: name, profile: shapeProfile(data) })
      }
      case 'market_movers': {
        const kind = String(args?.kind ?? 'gainers').toLowerCase()
        if (!['gainers', 'losers', 'active'].includes(kind)) {
          throw new Error('kind must be gainers, losers or active')
        }
        const data = await lane('movers', { kind })
        return ok({ tool: name, ...shapeMovers(data) })
      }
      case 'market_sectors': {
        const data = await lane('sectors', { date: args?.date, exchange: args?.exchange })
        return ok({ tool: name, sectors: data?.sectors || [], ...stamp(data?.source) })
      }
      case 'market_news': {
        const scope = String(args?.scope ?? (args?.symbols ? 'symbols' : 'market')).toLowerCase()
        const params = { scope, limit: args?.limit }
        if (scope === 'symbols') params.symbols = symbolsArg(args)
        const data = await lane('news', params)
        return ok({ tool: name, scope, ...shapeNews(data, args?.limit) })
      }
      case 'market_fundamentals': {
        const data = await lane('fundamentals', { symbol: symbolArg(args) })
        return ok({ tool: name, fundamentals: shapeFundamentals(data) })
      }
      case 'market_earnings': {
        const data = await lane('earnings', { symbol: symbolArg(args), limit: args?.limit })
        return ok({ tool: name, symbol: data?.symbol, events: data?.events || [], ...stamp(data?.source) })
      }
      case 'market_dividends': {
        const data = await lane('dividends', { symbol: symbolArg(args), limit: args?.limit })
        return ok({ tool: name, symbol: data?.symbol, events: data?.events || [], ...stamp(data?.source) })
      }
      case 'market_status': {
        const data = await lane('status', { exchange: args?.exchange })
        return ok({ tool: name, sessions: data?.sessions || [], ...stamp(data?.source) })
      }
      case 'market_macro': {
        const series = (args?.name ?? '').toString().trim()
        if (!series) throw new Error('name is required')
        const data = await lane('macro', { name: series, from: args?.from, to: args?.to })
        return ok({
          tool: name,
          series: data?.name,
          unit: data?.unit,
          interval: data?.interval,
          points: (data?.points || []).slice(-60),
          ...stamp(data?.source),
        })
      }
      case 'market_research_start': {
        const kind = String(args?.kind ?? 'equity_brief').trim().toLowerCase()
        if (!['equity_brief', 'enrichment', 'risk_rubric'].includes(kind)) {
          throw new Error('kind must be equity_brief, enrichment or risk_rubric')
        }
        const body = {
          kind,
          symbol: symbolArg(args),
          as_of: args?.as_of,
          effort: args?.effort,
          rubric_version: args?.rubric_version,
          dimensions: Array.isArray(args?.dimensions) ? args.dimensions : undefined,
        }
        const data = await lane('research/start', {}, { method: 'POST', body })
        return ok({ tool: name, research: shapeResearch(data) })
      }
      case 'market_research_get': {
        const id = String(args?.run_id ?? '').trim()
        if (!id) throw new Error('run_id is required')
        const data = await lane(`research/${encodeURIComponent(id)}`)
        return ok({ tool: name, research: shapeResearch(data) })
      }
      case 'market_verify_facts': {
        const fields = Array.isArray(args?.fields)
          ? args.fields.map((field) => String(field).trim()).filter(Boolean)
          : String(args?.fields ?? '').split(',').map((field) => field.trim()).filter(Boolean)
        if (fields.length === 0) throw new Error('fields is required')
        const data = await lane('research/verify', {}, {
          method: 'POST',
          body: { symbol: symbolArg(args), fields, as_of: args?.as_of },
        })
        return ok({
          tool: name,
          verification: data?.data,
          cache_hit: data?.meta?.cache_hit === true,
          retrieved_at: data?.meta?.retrieved_at,
          synthesis_note: 'generated verification synthesis — conflicts and inconclusive fields must remain explicit',
        })
      }
      default:
        throw new Error(`unknown tool: ${name}`)
    }
  } catch (err) {
    const message = err?.message ?? String(err)
    const extra = {}
    if (err?.kind) extra.kind = err.kind
    if (err?.retryAfter) extra.retry_after_seconds = err.retryAfter
    return fail(name, message, extra)
  }
}

// ── tool descriptors (advertised to the MCP client; MUST match the manifest) ──
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })

export const tools = [
  {
    name: 'market_quote',
    description:
      'Live price for one instrument — stocks, indexes (^GSPC), crypto (BTCUSD), forex (EURUSD) and commodities (GCUSD) all work. Returns price, change, day/year range, volume, market cap and the extended-hours quote when there is one, with the as-of time and the data source named. Read-only. args: symbol (required)',
    inputSchema: A({ symbol: S('ticker, e.g. AAPL, ^GSPC, BTCUSD, EURUSD') }, ['symbol']),
  },
  {
    name: 'market_quotes',
    description:
      'Live prices for several instruments in ONE call — use this instead of repeating market_quote when comparing or watching a list. Read-only. args: symbols (required, array or comma-separated)',
    inputSchema: A({ symbols: S('comma-separated tickers, e.g. "AAPL,MSFT,^GSPC"') }, ['symbols']),
  },
  {
    name: 'market_series',
    description:
      'Price history for a symbol over a range, summarised for reading: open/close/high/low, the change over the range, and a sampled path of closes. Resolution is chosen from the range. Read-only. args: symbol (required), range? (1D|5D|1M|6M|YTD|1Y|5Y|MAX, default 1M)',
    inputSchema: A({ symbol: S('ticker'), range: S('1D, 5D, 1M, 6M, YTD, 1Y, 5Y or MAX (default 1M)') }, ['symbol']),
  },
  {
    name: 'market_search',
    description:
      'Find a ticker by company or instrument name when you do not already know the symbol. Read-only. args: query (required), limit?',
    inputSchema: A({ query: S('company or instrument name'), limit: N('1-50, default 10') }, ['query']),
  },
  {
    name: 'market_profile',
    description:
      'Company reference data for a symbol: name, exchange, sector, industry, country, CEO, employees, IPO date, market cap, beta and the business description. Read-only. args: symbol (required)',
    inputSchema: A({ symbol: S('ticker') }, ['symbol']),
  },
  {
    name: 'market_movers',
    description:
      "Today's ranked US market lists: biggest gainers, biggest losers, or most actively traded. Read-only. args: kind? (gainers|losers|active, default gainers)",
    inputSchema: A({ kind: S('gainers (default), losers or active') }),
  },
  {
    name: 'market_sectors',
    description:
      'Sector performance for a session — the average change per sector, for reading how the market moved beneath the index. Read-only. args: date? (YYYY-MM-DD, default the latest session), exchange?',
    inputSchema: A({ date: S('YYYY-MM-DD, default latest'), exchange: S('e.g. NASDAQ') }),
  },
  {
    name: 'market_news',
    description:
      'Market news with source and publication time — the whole market, the stock tape, company press releases, or the stream for specific symbols. Some stories carry a sentiment score. Read-only. args: scope? (market|stocks|press|symbols), symbols? (required when scope=symbols), limit?',
    inputSchema: A({
      scope: S('market (default), stocks, press or symbols'),
      symbols: S('comma-separated tickers, required when scope=symbols'),
      limit: N('1-100, default 20'),
    }),
  },
  {
    name: 'market_fundamentals',
    description:
      'Trailing-twelve-month fundamentals and analyst consensus for a symbol: market cap, P/E, price-to-book, price-to-sales, dividend yield, EPS, margins, leverage, plus the analyst grade split and price targets. Read-only. args: symbol (required)',
    inputSchema: A({ symbol: S('ticker') }, ['symbol']),
  },
  {
    name: 'market_earnings',
    description:
      'Reported and scheduled earnings for a symbol: date, actual and estimated EPS and revenue. Read-only. args: symbol (required), limit?',
    inputSchema: A({ symbol: S('ticker'), limit: N('1-100, default 20') }, ['symbol']),
  },
  {
    name: 'market_dividends',
    description:
      'Dividend history for a symbol: ex-date, payment date, amount, yield and frequency. Read-only. args: symbol (required), limit?',
    inputSchema: A({ symbol: S('ticker'), limit: N('1-100, default 20') }, ['symbol']),
  },
  {
    name: 'market_status',
    description:
      'Whether markets are open or closed right now, with local session hours by region or exchange. Check this before describing a price as live. Read-only. args: exchange? (e.g. NASDAQ; omit for the global board)',
    inputSchema: A({ exchange: S('e.g. NASDAQ; omit for every region') }),
  },
  {
    name: 'market_macro',
    description:
      'An economic series by name — GDP, CPI, inflationRate, unemploymentRate, federalFunds, retailSales, consumerSentiment and the other published indicators. Read-only. args: name (required), from?, to?',
    inputSchema: A(
      { name: S('e.g. GDP, CPI, inflationRate, unemploymentRate, federalFunds'), from: S('YYYY-MM-DD'), to: S('YYYY-MM-DD') },
      ['name'],
    ),
  },
  {
    name: 'market_research_start',
    description:
      'Start on-demand grounded research for one public company. Use equity_brief for bull/bear debates, KPIs, peer read-throughs and management commentary; enrichment for typed facts and ownership filings; risk_rubric for a versioned evidence-based rubric. This is asynchronous and never investment advice. Read-only. args: symbol (required), kind?, effort?, as_of?, rubric_version?, dimensions?',
    inputSchema: A(
      {
        symbol: S('ticker'),
        kind: S('equity_brief (default), enrichment or risk_rubric'),
        effort: S('minimal, low, medium (default), high or xhigh'),
        as_of: S('ISO date or explicit research cutoff'),
        rubric_version: S('version label for risk_rubric'),
        dimensions: { type: 'array', items: S('lowercase rubric dimension name'), description: 'up to 8 rubric dimensions' },
      },
      ['symbol'],
    ),
  },
  {
    name: 'market_research_get',
    description:
      'Get an asynchronous grounded finance research run. Terminal output.grounding is authoritative; output text and structured fields are generated synthesis. Read-only. args: run_id (required)',
    inputSchema: A({ run_id: S('run identifier returned by market_research_start') }, ['run_id']),
  },
  {
    name: 'market_verify_facts',
    description:
      'Cross-check selected financial facts against issuer, regulator, exchange and other authoritative evidence without replacing canonical market data. Returns verified, conflict or inconclusive states with grounding. Read-only. args: symbol (required), fields (required), as_of?',
    inputSchema: A(
      {
        symbol: S('ticker'),
        fields: { type: 'array', items: S('field name such as revenue, eps, market_cap or next_earnings_date') },
        as_of: S('ISO date or explicit research cutoff'),
      },
      ['symbol', 'fields'],
    ),
  },
]

export const TOOL_NAMES = tools.map((t) => t.name)

// ── JSON-RPC stdio server ─────────────────────────────────────────────────────
const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? '2024-11-05',
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    try {
      return await dispatch(name, args)
    } catch (err) {
      return fail(name, err?.message ?? String(err))
    }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n')
}
const rpcOk = (id, result) => ({ jsonrpc: '2.0', id, result })
const rpcErr = (id, code, message) => ({ jsonrpc: '2.0', id, error: { code, message } })

function startStdioServer() {
  const rl = createInterface({ input: process.stdin })
  rl.on('line', async (line) => {
    if (!line.trim()) return
    let req
    try {
      req = JSON.parse(line)
    } catch (err) {
      send(rpcErr(null, -32700, 'parse error: ' + err.message))
      return
    }
    const fn = handlers[req.method]
    if (!fn) {
      if (req.id !== undefined) send(rpcErr(req.id, -32601, `method not found: ${req.method}`))
      return
    }
    try {
      const result = await fn(req.params)
      if (req.id !== undefined && result !== null) send(rpcOk(req.id, result))
    } catch (err) {
      if (req.id !== undefined) send(rpcErr(req.id, -32000, err?.message ?? String(err)))
    }
  })
  process.stdin.on('end', () => process.exit(0))
  process.on('SIGINT', () => process.exit(0))
  process.on('SIGTERM', () => process.exit(0))
}

// `--selftest`: list the registry, exercise the pure shaping, then verify the
// registry against every agent manifest that ships a finance server.
// executor/mcp Manager.verifyTools makes any bridge<->manifest tool-set drift a
// FATAL daemon boot; this guard turns the same drift into a non-zero exit at
// build/CI time so it never reaches the fleet. Offline: reads only the local
// registry + agents/*.json (no network).
function runSelftest() {
  console.log(`finance: ${tools.length} tools (lane=${configured() ? 'configured' : 'not configured'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  // Absent figures must stay absent: a vendor that sends no market cap must not
  // become a company worth $0.
  const sparse = shapeQuote({ symbol: 'X', price: 10, market_cap: null, volume: 0, source: { provider: 'fmp' } })
  if ('market_cap' in sparse) {
    console.error('finance SELFTEST FAILED: an absent market cap became a value')
    process.exit(1)
  }
  if (sparse.volume !== 0) {
    console.error('finance SELFTEST FAILED: a real zero volume was dropped')
    process.exit(1)
  }
  if (sparse.source !== 'fmp') {
    console.error('finance SELFTEST FAILED: the answering provider was not carried through')
    process.exit(1)
  }

  // A staleness note must survive shaping — the model has to be able to say so.
  const stale = shapeQuote({ symbol: 'X', price: 1, source: { provider: 'fmp', stale: true } })
  if (!stale.note || !stale.note.includes('last good value')) {
    console.error('finance SELFTEST FAILED: staleness was not carried to the model')
    process.exit(1)
  }

  // The series shaping must summarise rather than dump, and must not invent.
  const candles = []
  for (let i = 0; i < 500; i++) {
    candles.push({ t: `2026-01-01T00:${String(i % 60).padStart(2, '0')}:00Z`, o: 10 + i, h: 12 + i, l: 9 + i, c: 11 + i, v: 100 })
  }
  const series = shapeSeries({ symbol: 'X', interval: '1min', candles, source: { provider: 'fmp' } })
  if (!series || series.bars !== 500) {
    console.error('finance SELFTEST FAILED: series shaping lost the bar count')
    process.exit(1)
  }
  if (series.points.length > SERIES_POINTS + 2) {
    console.error(`finance SELFTEST FAILED: series returned ${series.points.length} points, not a summary`)
    process.exit(1)
  }
  if (series.high !== 511 || series.low !== 9 || series.open !== 10 || series.close !== 510) {
    console.error('finance SELFTEST FAILED: series extremes are wrong')
    process.exit(1)
  }
  if (series.total_volume !== 50000) {
    console.error('finance SELFTEST FAILED: series volume did not sum')
    process.exit(1)
  }
  if (shapeSeries({ symbol: 'X', candles: [], source: {} }) !== null) {
    console.error('finance SELFTEST FAILED: an empty series was shaped into data')
    process.exit(1)
  }

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.FINANCE_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`finance SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`finance FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'finance')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`finance FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`finance: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`finance SELFTEST FAILED: no manifest under ${agentsDir} declares a finance server`)
    process.exit(1)
  }
  if (drift) {
    console.error('finance SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`finance OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
