'use client'

/**
 * Market hooks over the router's finance lane.
 *
 * Cadence follows the data, not the panel: a live quote refetches on a short
 * beat, a daily chart on a long one, a company profile hardly at all. Nothing
 * polls while the tab is hidden, and nothing polls while the route is unmounted
 * — an app session that never opens markets costs nothing.
 *
 * Failures are VALUES here, not thrown states: the router answers a throttle, an
 * outage or a missing key with a typed, plain-language reason, and the panels
 * render that reason instead of a blank.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { qk } from '@/lib/query/keys'
import {
  financeFailure,
  cancelFinanceResearch,
  extractFinanceNews,
  getFinanceResearch,
  getBoard,
  getDividends,
  getEarnings,
  getMarketStatus,
  getMarketsHome,
  getNews,
  getQuote,
  getQuotes,
  getSeries,
  getSymbolPage,
  searchSymbols,
  startFinanceResearch,
  verifyFinanceFacts,
  type AssetClass,
  type FinanceFailure,
  type FinanceResearchKind,
  type MarketsHome,
  type NewsScope,
  type Range,
  type SymbolPage,
} from '@/lib/api/finance'

/** Refetch beats, in ms, by how fast the underlying data actually moves. */
const BEAT = {
  /** Live prices — the stats bar and the strip. */
  live: 15_000,
  /** Intraday charts: a bar cannot change faster than its own width. */
  intraday: 60_000,
  /** Settled series and ranked lists. */
  slow: 5 * 60_000,
  /** News. */
  news: 3 * 60_000,
  /** Reference data — profile, fundamentals, dividends. */
  reference: 30 * 60_000,
} as const

/** A chart range is intraday when its bars are. */
function isIntraday(range: Range): boolean {
  return range === '1D' || range === '5D' || range === '1M'
}

function retryFinance(failureCount: number, error: unknown): boolean {
  const failure = financeFailure(error)
  if (!failure) return failureCount < 1
  return failureCount < 1 && (failure.kind === 'upstream' || failure.kind === 'timeout')
}

/**
 * Shared query options. `refetchIntervalInBackground: false` is the rule that
 * keeps a forgotten tab from spending the shared vendor quota all afternoon.
 */
function beat(interval: number) {
  return {
    refetchInterval: interval,
    refetchIntervalInBackground: false,
    staleTime: Math.floor(interval / 2),
    retry: retryFinance,
  } as const
}

/** The markets home, in one call, each panel independently answered. */
export function useMarketsHome(symbols: string[] = [], enabled = true) {
  return useQuery<MarketsHome>({
    queryKey: qk.financeHome(symbols),
    queryFn: ({ signal }) => getMarketsHome(symbols.length ? symbols : undefined, signal),
    enabled,
    ...beat(BEAT.live),
  })
}

/** A symbol's whole opening state, in one call. */
export function useSymbolPage(symbol: string, range: Range, enabled = true) {
  return useQuery<SymbolPage>({
    queryKey: qk.financeSymbol(symbol, range),
    queryFn: ({ signal }) => getSymbolPage(symbol, range, signal),
    enabled: enabled && symbol.trim() !== '',
    ...beat(isIntraday(range) ? BEAT.live : BEAT.slow),
  })
}

/** One live quote. */
export function useQuote(symbol: string, enabled = true) {
  return useQuery({
    queryKey: qk.financeQuote(symbol),
    queryFn: ({ signal }) => getQuote(symbol, signal),
    enabled: enabled && symbol.trim() !== '',
    ...beat(BEAT.live),
  })
}

/** Many live quotes in one call — a watchlist or a strip. */
export function useQuotes(symbols: string[], enabled = true) {
  return useQuery({
    queryKey: qk.financeQuotes(symbols),
    queryFn: ({ signal }) => getQuotes(symbols, signal),
    enabled: enabled && symbols.length > 0,
    ...beat(BEAT.live),
  })
}

/** A chart series at a range. */
export function useSeries(symbol: string, range: Range, enabled = true) {
  return useQuery({
    queryKey: qk.financeSeries(symbol, range),
    queryFn: ({ signal }) => getSeries(symbol, range, signal),
    enabled: enabled && symbol.trim() !== '',
    ...beat(isIntraday(range) ? BEAT.intraday : BEAT.slow),
  })
}

/** A whole asset class in one call — indexes, crypto or commodities. */
export function useBoard(assetClass: AssetClass, enabled = true) {
  return useQuery({
    queryKey: qk.financeBoard(assetClass),
    queryFn: ({ signal }) => getBoard(assetClass, signal),
    enabled,
    ...beat(BEAT.live),
  })
}

/** A news stream. */
export function useNews(scope: NewsScope, symbols?: string[], enabled = true) {
  return useQuery({
    queryKey: qk.financeNews(scope, symbols),
    queryFn: ({ signal }) => getNews(scope, { symbols, limit: 20 }, signal),
    enabled: enabled && (scope !== 'symbols' || (symbols?.length ?? 0) > 0),
    ...beat(BEAT.news),
  })
}

/** Symbol search. Only runs once the query is worth asking about. */
export function useSymbolSearch(query: string) {
  const trimmed = query.trim()
  return useQuery({
    queryKey: qk.financeSearch(trimmed),
    queryFn: ({ signal }) => searchSymbols(trimmed, 12, signal),
    enabled: trimmed.length >= 2,
    staleTime: 5 * 60_000,
    retry: 0,
  })
}

/** Earnings history. Reference data: fetched when its tab opens, rarely after. */
export function useEarnings(symbol: string, enabled = true) {
  return useQuery({
    queryKey: qk.financeEarnings(symbol),
    queryFn: ({ signal }) => getEarnings(symbol, 20, signal),
    enabled: enabled && symbol.trim() !== '',
    ...beat(BEAT.reference),
  })
}

/** Dividend history. */
export function useDividends(symbol: string, enabled = true) {
  return useQuery({
    queryKey: qk.financeDividends(symbol),
    queryFn: ({ signal }) => getDividends(symbol, 20, signal),
    enabled: enabled && symbol.trim() !== '',
    ...beat(BEAT.reference),
  })
}

/** Whether markets are open. */
export function useMarketStatus(enabled = true) {
  return useQuery({
    queryKey: qk.financeStatus(),
    queryFn: ({ signal }) => getMarketStatus(undefined, signal),
    enabled,
    ...beat(BEAT.news),
  })
}

export function useStartFinanceResearch() {
  return useMutation({
    mutationFn: (input: {
      kind: FinanceResearchKind
      symbol: string
      rubric_version?: string
      dimensions?: string[]
    }) => startFinanceResearch({ ...input, effort: 'medium' }),
  })
}

export function useFinanceResearch(runId: string | null) {
  return useQuery({
    queryKey: qk.financeResearch(runId ?? ''),
    queryFn: ({ signal }) => getFinanceResearch(runId as string, signal),
    enabled: Boolean(runId),
    refetchInterval: (query) => {
      const status = query.state.data?.run.status
      return status === 'queued' || status === 'running' ? 2_000 : false
    },
    refetchIntervalInBackground: false,
    staleTime: 0,
    retry: retryFinance,
  })
}

export function useCancelFinanceResearch() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: cancelFinanceResearch,
    onSuccess: (result) => {
      queryClient.setQueryData(qk.financeResearch(result.run.id), result)
    },
  })
}

export function useVerifyFinanceFacts() {
  return useMutation({ mutationFn: verifyFinanceFacts })
}

export function useExtractFinanceNews() {
  return useMutation({ mutationFn: extractFinanceNews })
}

/**
 * The typed reason a query failed, or null. Panels render this instead of a
 * blank so a vendor outage reads as an explanation rather than an absence.
 */
export function failureOf(error: unknown): FinanceFailure | null {
  return financeFailure(error)
}
