/**
 * Finance resource — the market lane.
 *
 * Every call here goes to the ROUTER's /finance/* surface, never to a market
 * data vendor. The router holds the vendor keys, caches by freshness class,
 * collapses concurrent identical requests, rate-limits per user and counts every
 * upstream call — so the browser and Neo's finance tools share one cache, one
 * quota and one bill, and no vendor key is ever shipped to a browser bundle.
 *
 * Two shapes govern everything below:
 *
 *   - Absent means absent. Optional numbers arrive as `undefined` when the
 *     vendor had nothing, never as 0. The UI must render "—", not "$0".
 *   - Every payload carries its `source`: which provider answered, when it was
 *     fetched, whether that was the backup provider, and whether the value is a
 *     stale one being served because the provider stopped answering.
 */
import { apiFetch, ApiError } from '@/lib/api/client'

/* ── the wire model ───────────────────────────────────────────────────────── */

export type FinanceProvider = 'fmp' | 'alphavantage'

export interface FinanceSource {
  provider: FinanceProvider
  fetched_at: string
  /** The backup provider answered because the primary could not. */
  fallback?: boolean
  /** The last good value, served because the provider is not answering now. */
  stale?: boolean
}

export type AssetClass = 'equity' | 'index' | 'crypto' | 'forex' | 'commodity'

export interface ExtendedQuote {
  session?: string
  price?: number
  change?: number
  change_percent?: number
  bid_price?: number
  ask_price?: number
  volume?: number
  as_of?: string
}

export interface Quote {
  symbol: string
  name?: string
  exchange?: string
  class?: AssetClass
  currency?: string
  price?: number
  change?: number
  change_percent?: number
  open?: number
  day_high?: number
  day_low?: number
  previous_close?: number
  year_high?: number
  year_low?: number
  volume?: number
  avg_volume?: number
  market_cap?: number
  price_avg_50?: number
  price_avg_200?: number
  as_of?: string
  extended?: ExtendedQuote
  source: FinanceSource
}

export interface QuoteBoard {
  quotes: Quote[]
  source: FinanceSource
}

export interface PriceChange {
  symbol: string
  windows: Record<string, number>
  source: FinanceSource
}

export type Interval =
  | '1min'
  | '5min'
  | '15min'
  | '30min'
  | '1hour'
  | '4hour'
  | '1day'
  | '1week'
  | '1month'

/** One OHLCV bar. Keys are short because a long series is mostly this shape. */
export interface Candle {
  t: string
  o: number
  h: number
  l: number
  c: number
  v?: number
}

export interface Series {
  symbol: string
  interval: Interval
  candles: Candle[]
  source: FinanceSource
}

export interface Profile {
  symbol: string
  name?: string
  exchange?: string
  exchange_name?: string
  currency?: string
  sector?: string
  industry?: string
  country?: string
  ceo?: string
  employees?: number
  website?: string
  description?: string
  image_url?: string
  ipo_date?: string
  market_cap?: number
  beta?: number
  is_etf?: boolean
  is_fund?: boolean
  is_active?: boolean
  range?: string
  source: FinanceSource
}

export interface SearchMatch {
  symbol: string
  name?: string
  exchange?: string
  exchange_name?: string
  currency?: string
  class?: AssetClass
}

export interface SearchResults {
  query: string
  matches: SearchMatch[]
  source: FinanceSource
}

export type MoverKind = 'gainers' | 'losers' | 'active'

export interface Mover {
  symbol: string
  name?: string
  exchange?: string
  price?: number
  change?: number
  change_percent?: number
  volume?: number
}

export interface MoverList {
  kind: MoverKind
  movers: Mover[]
  source: FinanceSource
}

export interface SectorPerformance {
  sector: string
  exchange?: string
  change_percent: number
  date?: string
}

export interface SectorBoard {
  sectors: SectorPerformance[]
  source: FinanceSource
}

export interface NewsItem {
  title: string
  url: string
  publisher?: string
  site?: string
  summary?: string
  image_url?: string
  symbols?: string[]
  published_at?: string
  sentiment?: { score: number; label?: string }
}

export interface NewsFeed {
  items: NewsItem[]
  source: FinanceSource
}

export interface Analysts {
  strong_buy?: number
  buy?: number
  hold?: number
  sell?: number
  strong_sell?: number
  consensus?: string
  target_high?: number
  target_low?: number
  target_median?: number
  target_mean?: number
}

export interface FundamentalSummary {
  symbol: string
  market_cap?: number
  pe_ratio?: number
  forward_pe?: number
  price_to_book?: number
  price_to_sales?: number
  dividend_yield?: number
  eps?: number
  return_on_equity?: number
  net_profit_margin?: number
  debt_to_equity?: number
  current_ratio?: number
  enterprise_value?: number
  free_cash_flow_yield?: number
  analysts?: Analysts
  source: FinanceSource
}

export interface EarningsEvent {
  symbol: string
  date: string
  eps_actual?: number
  eps_estimated?: number
  revenue_actual?: number
  revenue_estimated?: number
}

export interface EarningsHistory {
  symbol: string
  events: EarningsEvent[]
  source: FinanceSource
}

export interface DividendEvent {
  symbol: string
  date: string
  payment_date?: string
  record_date?: string
  amount?: number
  yield?: number
  frequency?: string
}

export interface DividendHistory {
  symbol: string
  events: DividendEvent[]
  source: FinanceSource
}

export interface MarketSession {
  exchange: string
  name?: string
  region?: string
  timezone?: string
  open_time?: string
  close_time?: string
  is_open: boolean
  note?: string
}

export interface MarketStatus {
  sessions: MarketSession[]
  source: FinanceSource
}

export interface EconomicSeries {
  name: string
  unit?: string
  interval?: string
  points: { date: string; value: number }[]
  source: FinanceSource
}

export type FinanceResearchKind = 'equity_brief' | 'enrichment' | 'risk_rubric'

export interface ResearchCitation {
  url: string
  title?: string
}

export interface ResearchGrounding {
  field: string
  citations: ResearchCitation[]
  confidence?: string
}

export interface FinanceResearchOutput {
  text?: string
  structured?: unknown
  content?: unknown
  grounding?: ResearchGrounding[]
}

export interface FinanceResearchRun {
  id: string
  status: 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'
  createdAt?: string
  updatedAt?: string
  output?: FinanceResearchOutput
  costDollars?: { total: number }
  error?: { code?: string; message?: string }
}

export interface FinanceResearchEnvelope {
  run: FinanceResearchRun
  workflow?: string
  subject?: string
  meta: { cache_hit?: boolean; retrieved_at?: string }
}

export interface FinanceVerificationResult {
  data: {
    requestId: string
    searchType?: string
    results: Array<{
      title: string
      url: string
      publishedDate?: string
      highlights?: string[]
    }>
    output?: FinanceResearchOutput
    costDollars?: { total: number }
  }
  meta: { cache_hit?: boolean; retrieved_at?: string }
}

export interface FinanceNewsEvidenceResult {
  data: {
    requestId: string
    results: Array<{
      title: string
      url: string
      publishedDate?: string
      highlights?: string[]
    }>
    statuses: Array<{
      id: string
      status: string
      error?: { tag: string; httpStatusCode?: number }
    }>
    costDollars?: { total: number }
  }
  meta: { cache_hit?: boolean; retrieved_at?: string }
  error?: { kind: string; message: string }
}

/* ── failure, as a value ──────────────────────────────────────────────────── */

export type FinanceFailureKind =
  | 'not_configured'
  | 'throttled'
  | 'rate_limited'
  | 'upstream'
  | 'timeout'
  | 'not_found'
  | 'bad_request'

export interface FinanceFailure {
  kind: FinanceFailureKind
  message: string
  provider?: FinanceProvider
  retry_after_seconds?: number
}

/**
 * A panel's outcome. Data or an honest reason — never a silent blank, and never
 * an empty object rendered as though it were real.
 */
export type Panel<T> = { data: T; error?: undefined } | { data?: undefined; error: FinanceFailure }

/** Read the typed failure out of an ApiError the router raised. */
export function financeFailure(err: unknown): FinanceFailure | null {
  if (!(err instanceof ApiError)) return null
  const body = err.body as { error?: FinanceFailure } | undefined
  if (body && body.error && typeof body.error.message === 'string') return body.error
  return { kind: 'upstream', message: 'Market data could not be loaded.' }
}

/** The composite pages report per-panel outcomes in this envelope. */
interface PanelsResponse {
  panels: Record<string, { data?: unknown; error?: FinanceFailure }>
}

function panelOf<T>(res: PanelsResponse, name: string): Panel<T> {
  const raw = res.panels?.[name]
  if (!raw) {
    return { error: { kind: 'not_found', message: 'This panel was not returned.' } }
  }
  if (raw.error) return { error: raw.error }
  return { data: raw.data as T }
}

/* ── ranges ───────────────────────────────────────────────────────────────── */

export const RANGES = ['1D', '5D', '1M', '6M', 'YTD', '1Y', '5Y', 'MAX'] as const
export type Range = (typeof RANGES)[number]

/* ── calls ────────────────────────────────────────────────────────────────── */

function q(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '') continue
    search.set(k, String(v))
  }
  const s = search.toString()
  return s ? `?${s}` : ''
}

export function getQuote(symbol: string, signal?: AbortSignal): Promise<Quote> {
  return apiFetch<Quote>(`/finance/quote${q({ symbol })}`, { signal })
}

export function getQuotes(symbols: string[], signal?: AbortSignal): Promise<QuoteBoard> {
  return apiFetch<QuoteBoard>(`/finance/quotes${q({ symbols: symbols.join(',') })}`, { signal })
}

export function getSeries(symbol: string, range: Range, signal?: AbortSignal): Promise<Series> {
  return apiFetch<Series>(`/finance/series${q({ symbol, range })}`, { signal })
}

export function getProfile(symbol: string, signal?: AbortSignal): Promise<Profile> {
  return apiFetch<Profile>(`/finance/profile${q({ symbol })}`, { signal })
}

export function searchSymbols(
  query: string,
  limit = 10,
  signal?: AbortSignal,
): Promise<SearchResults> {
  return apiFetch<SearchResults>(`/finance/search${q({ q: query, limit })}`, { signal })
}

export function getMovers(kind: MoverKind, signal?: AbortSignal): Promise<MoverList> {
  return apiFetch<MoverList>(`/finance/movers${q({ kind })}`, { signal })
}

export function getSectors(signal?: AbortSignal): Promise<SectorBoard> {
  return apiFetch<SectorBoard>('/finance/sectors', { signal })
}

export function getBoard(assetClass: AssetClass, signal?: AbortSignal): Promise<QuoteBoard> {
  return apiFetch<QuoteBoard>(`/finance/board${q({ class: assetClass })}`, { signal })
}

export type NewsScope = 'market' | 'stocks' | 'press' | 'symbols'

export function getNews(
  scope: NewsScope,
  opts: { symbols?: string[]; limit?: number } = {},
  signal?: AbortSignal,
): Promise<NewsFeed> {
  return apiFetch<NewsFeed>(
    `/finance/news${q({ scope, symbols: opts.symbols?.join(','), limit: opts.limit })}`,
    { signal },
  )
}

export function getFundamentals(symbol: string, signal?: AbortSignal): Promise<FundamentalSummary> {
  return apiFetch<FundamentalSummary>(`/finance/fundamentals${q({ symbol })}`, { signal })
}

export function getEarnings(
  symbol: string,
  limit = 20,
  signal?: AbortSignal,
): Promise<EarningsHistory> {
  return apiFetch<EarningsHistory>(`/finance/earnings${q({ symbol, limit })}`, { signal })
}

export function getDividends(
  symbol: string,
  limit = 20,
  signal?: AbortSignal,
): Promise<DividendHistory> {
  return apiFetch<DividendHistory>(`/finance/dividends${q({ symbol, limit })}`, { signal })
}

export function getMarketStatus(exchange?: string, signal?: AbortSignal): Promise<MarketStatus> {
  return apiFetch<MarketStatus>(`/finance/status${q({ exchange })}`, { signal })
}

export function getMacro(name: string, signal?: AbortSignal): Promise<EconomicSeries> {
  return apiFetch<EconomicSeries>(`/finance/macro${q({ name })}`, { signal })
}

export function startFinanceResearch(input: {
  kind: FinanceResearchKind
  symbol: string
  as_of?: string
  effort?: 'minimal' | 'low' | 'medium' | 'high' | 'xhigh'
  rubric_version?: string
  dimensions?: string[]
}): Promise<FinanceResearchEnvelope> {
  return apiFetch<FinanceResearchEnvelope>('/finance/research/start', {
    method: 'POST',
    body: JSON.stringify(input),
    timeoutMs: 30_000,
    retries: 0,
  })
}

export function getFinanceResearch(
  runId: string,
  signal?: AbortSignal,
): Promise<FinanceResearchEnvelope> {
  return apiFetch<FinanceResearchEnvelope>(`/finance/research/${encodeURIComponent(runId)}`, {
    signal,
  })
}

export function cancelFinanceResearch(runId: string): Promise<FinanceResearchEnvelope> {
  return apiFetch<FinanceResearchEnvelope>(
    `/finance/research/${encodeURIComponent(runId)}/cancel`,
    { method: 'POST', body: '{}', retries: 0 },
  )
}

export function verifyFinanceFacts(input: {
  symbol: string
  fields: string[]
  as_of?: string
}): Promise<FinanceVerificationResult> {
  return apiFetch<FinanceVerificationResult>('/finance/research/verify', {
    method: 'POST',
    body: JSON.stringify(input),
    timeoutMs: 30_000,
    retries: 0,
  })
}

export function extractFinanceNews(input: {
  symbol: string
  urls: string[]
}): Promise<FinanceNewsEvidenceResult> {
  return apiFetch<FinanceNewsEvidenceResult>('/finance/research/news', {
    method: 'POST',
    body: JSON.stringify(input),
    timeoutMs: 30_000,
    retries: 0,
  })
}

/* ── composites ───────────────────────────────────────────────────────────── */

export interface MarketsHome {
  strip: Panel<QuoteBoard>
  gainers: Panel<MoverList>
  losers: Panel<MoverList>
  active: Panel<MoverList>
  sectors: Panel<SectorBoard>
  status: Panel<MarketStatus>
  news: Panel<NewsFeed>
}

/** The markets home in ONE call, each panel independently answered. */
export async function getMarketsHome(
  symbols?: string[],
  signal?: AbortSignal,
): Promise<MarketsHome> {
  const res = await apiFetch<PanelsResponse>(`/finance/home${q({ symbols: symbols?.join(',') })}`, {
    signal,
  })
  return {
    strip: panelOf<QuoteBoard>(res, 'strip'),
    gainers: panelOf<MoverList>(res, 'gainers'),
    losers: panelOf<MoverList>(res, 'losers'),
    active: panelOf<MoverList>(res, 'active'),
    sectors: panelOf<SectorBoard>(res, 'sectors'),
    status: panelOf<MarketStatus>(res, 'status'),
    news: panelOf<NewsFeed>(res, 'news'),
  }
}

export interface SymbolPage {
  symbol: string
  range: Range
  quote: Panel<Quote>
  series: Panel<Series>
  profile: Panel<Profile>
  fundamentals: Panel<FundamentalSummary>
  extended: Panel<ExtendedQuote>
  change: Panel<PriceChange>
  news: Panel<NewsFeed>
}

/** A symbol's whole opening state in ONE call, each panel independent. */
export async function getSymbolPage(
  symbol: string,
  range: Range,
  signal?: AbortSignal,
): Promise<SymbolPage> {
  const res = await apiFetch<PanelsResponse & { symbol: string; range: Range }>(
    `/finance/symbol${q({ symbol, range })}`,
    { signal },
  )
  return {
    symbol: res.symbol ?? symbol,
    range: res.range ?? range,
    quote: panelOf<Quote>(res, 'quote'),
    series: panelOf<Series>(res, 'series'),
    profile: panelOf<Profile>(res, 'profile'),
    fundamentals: panelOf<FundamentalSummary>(res, 'fundamentals'),
    extended: panelOf<ExtendedQuote>(res, 'extended'),
    change: panelOf<PriceChange>(res, 'change'),
    news: panelOf<NewsFeed>(res, 'news'),
  }
}
