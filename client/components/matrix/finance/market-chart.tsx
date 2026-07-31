'use client'

/**
 * MarketChart — our chart, not a vendor's.
 *
 * Drawn in-repo as plain SVG from the dependencies already in the tree: no
 * charting package, no vendor script, no iframe. It reads as part of the app
 * because it uses the app's tokens — surfaces separate by background tone, never
 * by a border stroke; the single accent is Matrix Sage; there is no glow.
 *
 * What it carries:
 *   - two forms, area and candlestick, over the same series
 *   - a volume histogram beneath, when the series has volume
 *   - the range set 1D … MAX, each range fetched at its own resolution
 *   - a crosshair with an OHLC + volume readout at the cursor
 *   - a previous-close reference line, so "up today" is visible not inferred
 *   - colour by direction over the whole range
 *
 * It is operable without a mouse (the range buttons are buttons; ← / → move the
 * crosshair) and carries a text summary of the series for screen readers, since
 * an SVG path is not readable on its own.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useLocale, useTranslations } from 'next-intl'
import { cn } from '@/lib/utils'
import type { Range, Series } from '@/lib/api/finance'
import { RANGES } from '@/lib/api/finance'
import {
  formatCompact,
  formatDateTime,
  formatPercent,
  formatPrice,
  type Direction,
} from '@/lib/finance/format'

/** Up and down read the same everywhere in the app. */
const UP = 'oklch(0.72 0.14 155)'
const DOWN = 'oklch(0.62 0.2 25)'
const FLAT = 'oklch(0.62 0 0)'

export function directionColor(direction: Direction): string {
  return direction === 'up' ? UP : direction === 'down' ? DOWN : FLAT
}

export type ChartForm = 'area' | 'candles'

interface Geometry {
  width: number
  height: number
  padTop: number
  padBottom: number
  padLeft: number
  padRight: number
  volumeHeight: number
}

/** Where the price band ends and the volume band begins. */
function bands(g: Geometry) {
  const plotTop = g.padTop
  const plotBottom = g.height - g.padBottom - g.volumeHeight
  return { plotTop, plotBottom, volumeTop: plotBottom + 6, volumeBottom: g.height - g.padBottom }
}

export function MarketChart({
  series,
  range,
  onRangeChange,
  previousClose,
  form: formProp,
  onFormChange,
  height = 340,
  className,
  label,
}: {
  series: Series | undefined
  range: Range
  onRangeChange?: (range: Range) => void
  /** Yesterday's close, drawn as the reference the day's move is measured from. */
  previousClose?: number
  form?: ChartForm
  onFormChange?: (form: ChartForm) => void
  height?: number
  className?: string
  /** What the chart is OF, for the accessible summary (usually the ticker). */
  label?: string
}) {
  const t = useTranslations('finance')
  const locale = useLocale()
  const containerRef = useRef<HTMLDivElement>(null)
  const [measuredWidth, setMeasuredWidth] = useState(0)
  const [uncontrolledForm, setUncontrolledForm] = useState<ChartForm>('area')
  const form = formProp ?? uncontrolledForm
  const setForm = onFormChange ?? setUncontrolledForm

  const candles = useMemo(() => series?.candles ?? [], [series])
  const [cursor, setCursor] = useState<number | null>(null)
  const svgRef = useRef<SVGSVGElement>(null)

  // A range switch replaces the series under the cursor; a stale index would
  // read out a bar that is no longer there.
  useEffect(() => {
    setCursor(null)
  }, [range, series?.symbol])

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    const measure = () => setMeasuredWidth(container.getBoundingClientRect().width)
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0]
      setMeasuredWidth(entry?.contentRect.width ?? container.getBoundingClientRect().width)
    })
    observer.observe(container)
    return () => observer.disconnect()
  }, [])

  const width = Math.max(240, measuredWidth || 640)
  const hasVolume = candles.some((c) => typeof c.v === 'number')
  const geometry: Geometry = {
    width,
    height,
    padTop: 12,
    padBottom: 22,
    padLeft: 8,
    padRight: 56,
    volumeHeight: hasVolume ? Math.round(height * 0.18) : 0,
  }
  const { plotTop, plotBottom, volumeTop, volumeBottom } = bands(geometry)
  const plotWidth = Math.max(1, width - geometry.padLeft - geometry.padRight)
  const plotHeight = Math.max(1, plotBottom - plotTop)

  const scale = useMemo(() => {
    if (candles.length === 0) return null
    let low = candles[0].l
    let high = candles[0].h
    let maxVolume = 0
    for (const c of candles) {
      if (c.l < low) low = c.l
      if (c.h > high) high = c.h
      if (typeof c.v === 'number' && c.v > maxVolume) maxVolume = c.v
    }
    // The previous close belongs INSIDE the band, or its reference line would
    // sit off the chart exactly when the day gapped — the case it matters most.
    if (typeof previousClose === 'number' && Number.isFinite(previousClose)) {
      low = Math.min(low, previousClose)
      high = Math.max(high, previousClose)
    }
    // A perfectly flat series would divide by zero; give it a readable band.
    if (high === low) {
      const pad = Math.abs(high) * 0.01 || 1
      high += pad
      low -= pad
    }
    const span = high - low
    return {
      low,
      high,
      maxVolume,
      x: (i: number) =>
        candles.length === 1
          ? geometry.padLeft + plotWidth / 2
          : geometry.padLeft + (i / (candles.length - 1)) * plotWidth,
      y: (value: number) => plotBottom - ((value - low) / span) * plotHeight,
      vy: (value: number) =>
        maxVolume === 0
          ? volumeBottom
          : volumeBottom - (value / maxVolume) * (volumeBottom - volumeTop),
    }
  }, [
    candles,
    previousClose,
    geometry.padLeft,
    plotWidth,
    plotBottom,
    plotHeight,
    volumeTop,
    volumeBottom,
  ])

  const first = candles[0]
  const last = candles[candles.length - 1]
  const netChange = first && last ? last.c - first.o : undefined
  const direction: Direction =
    netChange === undefined || netChange === 0 ? 'flat' : netChange > 0 ? 'up' : 'down'
  const color = directionColor(direction)

  const areaPath = useMemo(() => {
    if (!scale || candles.length === 0) return { line: '', fill: '' }
    const points = candles.map((c, i) => `${scale.x(i).toFixed(2)},${scale.y(c.c).toFixed(2)}`)
    const line = `M${points.join('L')}`
    const fill = `${line}L${scale.x(candles.length - 1).toFixed(2)},${plotBottom}L${scale.x(0).toFixed(2)},${plotBottom}Z`
    return { line, fill }
  }, [scale, candles, plotBottom])

  // Candle width scales with density and never collapses to nothing.
  const candleWidth = Math.max(1, Math.min(12, (plotWidth / Math.max(1, candles.length)) * 0.7))

  const activeIndex = cursor ?? candles.length - 1
  const active = candles[activeIndex]

  const pointerToIndex = useCallback(
    (clientX: number): number | null => {
      const svg = svgRef.current
      if (!svg || candles.length === 0) return null
      const rect = svg.getBoundingClientRect()
      const ratio = (clientX - rect.left - geometry.padLeft) / plotWidth
      const index = Math.round(ratio * (candles.length - 1))
      if (!Number.isFinite(index)) return null
      return Math.min(candles.length - 1, Math.max(0, index))
    },
    [candles.length, geometry.padLeft, plotWidth],
  )

  const onKeyDown = (event: React.KeyboardEvent<SVGSVGElement>) => {
    if (candles.length === 0) return
    if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
      event.preventDefault()
      const step = event.key === 'ArrowLeft' ? -1 : 1
      const next = Math.min(candles.length - 1, Math.max(0, (cursor ?? candles.length - 1) + step))
      setCursor(next)
    } else if (event.key === 'Escape') {
      setCursor(null)
    }
  }

  // An SVG path says nothing to a screen reader; this does.
  const summary = useMemo(() => {
    if (!first || !last) return t('noPriceHistory')
    const changePercent = first.o !== 0 ? ((last.c - first.o) / first.o) * 100 : undefined
    const low = Math.min(...candles.map((c) => c.l))
    const high = Math.max(...candles.map((c) => c.h))
    return t('chartSummary', {
      label: label ?? series?.symbol ?? t('price'),
      range,
      open: formatPrice(first.o, undefined, locale),
      last: formatPrice(last.c, undefined, locale),
      direction:
        direction === 'up'
          ? t('directionUp')
          : direction === 'down'
            ? t('directionDown')
            : t('directionFlat'),
      change: formatPercent(changePercent, locale),
      low: formatPrice(low, undefined, locale),
      high: formatPrice(high, undefined, locale),
      count: candles.length,
    })
  }, [first, last, candles, direction, label, range, series?.symbol, t, locale])

  return (
    <div className={cn('flex w-full flex-col gap-2', className)}>
      {/* Controls — ranges and the form toggle. Chips separate by tone. */}
      <div className="flex flex-wrap items-center gap-1">
        <div
          className="flex flex-wrap items-center gap-0.5"
          role="group"
          aria-label={t('chartRange')}
        >
          {RANGES.map((r) => (
            <button
              key={r}
              type="button"
              onClick={() => onRangeChange?.(r)}
              aria-pressed={r === range}
              className={cn(
                'rounded-md px-2 py-1 font-mono text-[0.7rem] font-medium transition-colors',
                r === range
                  ? 'bg-muted text-foreground'
                  : 'text-muted-foreground hover:bg-muted/50 hover:text-foreground',
              )}
            >
              {r}
            </button>
          ))}
        </div>
        <div className="ml-auto flex items-center gap-0.5" role="group" aria-label={t('chartType')}>
          {(['area', 'candles'] as const).map((f) => (
            <button
              key={f}
              type="button"
              onClick={() => setForm(f)}
              aria-pressed={f === form}
              aria-label={f === 'area' ? t('lineChart') : t('candlestickChart')}
              className={cn(
                'rounded-md px-2 py-1 text-[0.7rem] font-medium transition-colors',
                f === form
                  ? 'bg-muted text-foreground'
                  : 'text-muted-foreground hover:bg-muted/50 hover:text-foreground',
              )}
            >
              {f === 'area' ? t('line') : t('candles')}
            </button>
          ))}
        </div>
      </div>

      {/* The readout — what is under the cursor, or the latest bar. */}
      <div className="text-muted-foreground flex min-h-[1.25rem] flex-wrap items-baseline gap-x-3 gap-y-0.5 font-mono text-[0.7rem]">
        {active ? (
          <>
            <span className="text-foreground">{formatDateTime(active.t, locale)}</span>
            <span>
              {t('openShort')} {formatPrice(active.o, undefined, locale)}
            </span>
            <span>
              {t('highShort')} {formatPrice(active.h, undefined, locale)}
            </span>
            <span>
              {t('lowShort')} {formatPrice(active.l, undefined, locale)}
            </span>
            <span style={{ color }}>
              {t('closeShort')} {formatPrice(active.c, undefined, locale)}
            </span>
            {typeof active.v === 'number' ? (
              <span>
                {t('volumeShort')} {formatCompact(active.v, locale)}
              </span>
            ) : null}
          </>
        ) : null}
      </div>

      <div ref={containerRef} className="w-full">
        <svg
          ref={svgRef}
          role="img"
          aria-label={summary}
          tabIndex={0}
          width="100%"
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          preserveAspectRatio="none"
          className="touch-none outline-none focus-visible:opacity-95"
          onPointerMove={(e) => setCursor(pointerToIndex(e.clientX))}
          onPointerLeave={() => setCursor(null)}
          onKeyDown={onKeyDown}
        >
          {scale && candles.length > 0 ? (
            <>
              {/* Horizontal guides — data chrome at low contrast, not depth. */}
              {[0, 0.25, 0.5, 0.75, 1].map((t) => {
                const y = plotTop + t * plotHeight
                return (
                  <line
                    key={t}
                    x1={geometry.padLeft}
                    x2={geometry.padLeft + plotWidth}
                    y1={y}
                    y2={y}
                    stroke="currentColor"
                    strokeWidth={1}
                    className="text-foreground/[0.06]"
                  />
                )
              })}
              {/* Price axis labels, right-hung so the series keeps the width. */}
              {[0, 0.5, 1].map((t) => {
                const value = scale.high - t * (scale.high - scale.low)
                const y = plotTop + t * plotHeight
                return (
                  <text
                    key={t}
                    x={geometry.padLeft + plotWidth + 6}
                    y={y + 3}
                    className="fill-muted-foreground font-mono text-[9px]"
                  >
                    {formatPrice(value, undefined, locale)}
                  </text>
                )
              })}

              {/* The previous close: the line today's move is measured from. */}
              {typeof previousClose === 'number' && Number.isFinite(previousClose) ? (
                <>
                  <line
                    x1={geometry.padLeft}
                    x2={geometry.padLeft + plotWidth}
                    y1={scale.y(previousClose)}
                    y2={scale.y(previousClose)}
                    stroke="currentColor"
                    strokeDasharray="3 4"
                    strokeWidth={1}
                    className="text-foreground/25"
                  />
                  <text
                    x={geometry.padLeft + plotWidth + 6}
                    y={scale.y(previousClose) + 3}
                    className="fill-muted-foreground font-mono text-[9px]"
                  >
                    {formatPrice(previousClose, undefined, locale)}
                  </text>
                </>
              ) : null}

              {/* Volume, beneath the price band. */}
              {hasVolume
                ? candles.map((c, i) => {
                    if (typeof c.v !== 'number') return null
                    const y = scale.vy(c.v)
                    const up = c.c >= c.o
                    return (
                      <rect
                        key={`v${i}`}
                        x={scale.x(i) - candleWidth / 2}
                        y={y}
                        width={candleWidth}
                        height={Math.max(0.5, volumeBottom - y)}
                        fill={up ? UP : DOWN}
                        opacity={0.28}
                      />
                    )
                  })
                : null}

              {form === 'area' ? (
                <>
                  <path d={areaPath.fill} fill={color} opacity={0.1} />
                  <path
                    d={areaPath.line}
                    fill="none"
                    stroke={color}
                    strokeWidth={1.75}
                    strokeLinejoin="round"
                    strokeLinecap="round"
                  />
                </>
              ) : (
                candles.map((c, i) => {
                  const up = c.c >= c.o
                  const barColor = up ? UP : DOWN
                  const bodyTop = scale.y(Math.max(c.o, c.c))
                  const bodyBottom = scale.y(Math.min(c.o, c.c))
                  return (
                    <g key={`c${i}`}>
                      <line
                        x1={scale.x(i)}
                        x2={scale.x(i)}
                        y1={scale.y(c.h)}
                        y2={scale.y(c.l)}
                        stroke={barColor}
                        strokeWidth={1}
                      />
                      <rect
                        x={scale.x(i) - candleWidth / 2}
                        y={bodyTop}
                        width={candleWidth}
                        height={Math.max(1, bodyBottom - bodyTop)}
                        fill={barColor}
                      />
                    </g>
                  )
                })
              )}

              {/* Crosshair. */}
              {cursor !== null && active ? (
                <g>
                  <line
                    x1={scale.x(cursor)}
                    x2={scale.x(cursor)}
                    y1={plotTop}
                    y2={volumeBottom}
                    stroke="currentColor"
                    strokeWidth={1}
                    className="text-foreground/30"
                  />
                  <circle cx={scale.x(cursor)} cy={scale.y(active.c)} r={3} fill={color} />
                </g>
              ) : null}
            </>
          ) : (
            <text
              x={width / 2}
              y={height / 2}
              textAnchor="middle"
              className="fill-muted-foreground text-[11px]"
            >
              {t('noPriceHistoryRange')}
            </text>
          )}
        </svg>
      </div>

      {/* The series in words, for anyone not reading the path. */}
      <p className="sr-only">{summary}</p>
    </div>
  )
}
