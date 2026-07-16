'use client'

/**
 * ConstructChart — the DATA-SIDE adapter. It maps the Construct `Chart` payload
 * (kind + series + points) onto the matching exact GAIA UI chart component's
 * props; it adds NO styling of its own — the charts render with GAIA's look,
 * fed by our data (invariant i2: the agent fills a trusted primitive).
 */
import { AreaChart } from '@/components/ui/area-chart'
import { BarChart } from '@/components/ui/bar-chart'
import { LineChart } from '@/components/ui/line-chart'
import { PieChart } from '@/components/ui/pie-chart'
import { RadarChart } from '@/components/ui/radar-chart'
import { ScatterChart } from '@/components/ui/scatter-chart'
import type { Chart, ChartPoint } from '@/lib/construct/types.gen'

/** Series keys: explicit `series` if given, else inferred from the first point. */
function seriesKeys(chart: Chart): string[] {
  if (chart.series && chart.series.length > 0) return chart.series.map((s) => s.key)
  const first = chart.points?.[0]?.values
  return first ? Object.keys(first) : []
}

/** Flatten ChartPoint[] into GAIA row data: { label, ...values }. */
function toRows(points: ChartPoint[]): Array<Record<string, string | number>> {
  return points.map((p, i) => ({ label: p.label ?? String(i), ...(p.values ?? {}) }))
}

/** Pass explicit series colours through only when every series defines one. */
function explicitColors(chart: Chart): string[] | undefined {
  if (chart.series && chart.series.length > 0 && chart.series.every((s) => !!s.color)) {
    return chart.series.map((s) => s.color as string)
  }
  return undefined
}

export function ConstructChart({ chart, title }: { chart: Chart; title?: string }) {
  const points = chart.points ?? []
  if (points.length === 0) return null
  const keys = seriesKeys(chart)
  if (keys.length === 0) return null

  const data = toRows(points)
  const colors = explicitColors(chart)

  switch (chart.kind) {
    case 'area':
      return <AreaChart data={data} xKey="label" yKeys={keys} title={title} colors={colors} />
    case 'bar':
      return (
        <BarChart
          data={data}
          xKey="label"
          yKeys={keys}
          title={title}
          colors={colors}
          variant={chart.stacked ? 'stacked' : 'default'}
        />
      )
    case 'line':
      return <LineChart data={data} xKey="label" yKeys={keys} title={title} colors={colors} />
    case 'pie': {
      const valueKey = keys[0]
      const pieData = points.map((p, i) => ({
        name: p.label ?? String(i),
        value: p.values?.[valueKey] ?? 0,
      }))
      return <PieChart data={pieData} nameKey="name" valueKey="value" title={title} mode="donut" />
    }
    case 'radar':
      return (
        <RadarChart data={data} angleKey="label" valueKeys={keys} title={title} colors={colors} />
      )
    case 'scatter':
      return <ScatterChart data={data} xKey={keys[0]} yKey={keys[1] ?? keys[0]} title={title} />
    default:
      return null
  }
}
