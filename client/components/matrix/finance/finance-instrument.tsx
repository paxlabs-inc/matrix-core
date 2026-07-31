'use client'

import type {
  ExtendedQuote,
  FinanceFailure,
  Profile,
  Quote,
  Range,
  Series,
} from '@/lib/api/finance'
import { MarketChart } from '@/components/matrix/finance/market-chart'
import { MarketIdentity, MarketQuoteBar } from '@/components/matrix/finance/market-quote-bar'
import { MarketUnavailable } from '@/components/matrix/finance/market-panel'

/** The shared quote-and-chart spine used by both /finance and Neo's Computer. */
export function FinanceInstrumentCore({
  quote,
  profile,
  extended,
  series,
  range,
  onRangeChange,
  seriesFailure,
  chartLabel,
}: {
  quote?: Quote
  profile?: Profile
  extended?: ExtendedQuote
  series?: Series
  range: Range
  onRangeChange?: (range: Range) => void
  seriesFailure?: FinanceFailure
  chartLabel: string
}) {
  return (
    <div data-finance-instrument className="flex flex-col gap-4">
      {quote ? (
        <>
          <MarketIdentity quote={quote} profile={profile} />
          <MarketQuoteBar quote={quote} extended={extended} />
        </>
      ) : null}
      {series ? (
        <MarketChart
          series={series}
          range={range}
          onRangeChange={onRangeChange}
          previousClose={quote?.previous_close}
          label={quote?.symbol ?? series.symbol}
        />
      ) : null}
      {seriesFailure ? <MarketUnavailable failure={seriesFailure} what={chartLabel} /> : null}
    </div>
  )
}
