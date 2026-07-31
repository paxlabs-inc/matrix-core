'use client'

import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from 'recharts'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/matrix/astryx-card'
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '@/components/ui/chart'
import { useTranslations } from 'next-intl'
import type { MetricPoint } from '@/lib/matrix-data'

/**
 * ThroughputChart — completed vs failed runs over time.
 * Backend-agnostic: driven entirely by the `data` prop.
 * Colors are resolved in JS (not CSS vars) per charting guidance.
 */
export function ThroughputChart({ data }: { data: MetricPoint[] }) {
  const t = useTranslations('throughputChart')

  const chartConfig = {
    completed: { label: t('completed'), color: '#004ced' },
    failed: { label: t('failed'), color: 'oklch(0.62 0.2 25)' },
  } satisfies ChartConfig

  return (
    <Card className="h-full">
      <CardHeader>
        <CardTitle className="text-base">{t('title')}</CardTitle>
        <CardDescription>{t('description')}</CardDescription>
      </CardHeader>
      <CardContent>
        <ChartContainer config={chartConfig} className="aspect-auto h-[200px] w-full sm:h-[240px]">
          <AreaChart data={data} margin={{ left: -12, right: 8, top: 4 }}>
            <defs>
              <linearGradient id="fillCompleted" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="#004ced" stopOpacity={0.35} />
                <stop offset="100%" stopColor="#004ced" stopOpacity={0} />
              </linearGradient>
            </defs>
            <CartesianGrid vertical={false} stroke="oklch(0.28 0.004 264)" />
            <XAxis
              dataKey="date"
              tickLine={false}
              axisLine={false}
              tickMargin={8}
              minTickGap={24}
              tickFormatter={(v: string) =>
                new Date(v).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })
              }
              className="font-mono text-xs"
            />
            <YAxis
              tickLine={false}
              axisLine={false}
              width={40}
              allowDecimals={false}
              className="font-mono text-xs"
            />
            <ChartTooltip cursor={false} content={<ChartTooltipContent indicator="line" />} />
            <Area
              dataKey="failed"
              type="monotone"
              stroke="oklch(0.62 0.2 25)"
              fill="transparent"
              strokeWidth={1.5}
              strokeDasharray="4 4"
            />
            <Area
              dataKey="completed"
              type="monotone"
              stroke="#004ced"
              fill="url(#fillCompleted)"
              strokeWidth={2}
            />
          </AreaChart>
        </ChartContainer>
      </CardContent>
    </Card>
  )
}
