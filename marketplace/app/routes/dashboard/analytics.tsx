import { useState } from "react";
import { Link } from "react-router";
import { ArrowLeft } from "lucide-react";
import AnimatedTabs from "../../../components/ui/smoothui/animated-tabs";
import type { Route } from "./+types/analytics";
import { getEnv } from "@/lib/env";
import { developerIdentityFor, getWallet, requireUser } from "@/lib/auth.server";
import { createDeusClient } from "@/lib/deus.server";
import type { AnalyticsPoint } from "@/lib/deus.types";
import { formatCount, formatPax, formatUptime, weiToPax } from "@/lib/format";
import { cn } from "@/lib/utils";
import { Stat, Surface } from "@/components/ui";

export async function loader({ request, params, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, { developer: developerIdentityFor(wallet) });
  const id = params.id;
  const [analytics, service] = await Promise.all([deus.analytics(id), deus.getService(id)]);
  return { analytics, service };
}

function shortDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

function BarChart({ series, metric }: { series: AnalyticsPoint[]; metric: "invocations" | "revenue" }) {
  const values = series.map((p) => (metric === "invocations" ? p.invocations : weiToPax(p.revenue_wei)));
  const max = Math.max(1, ...values);
  const labelCount = Math.min(5, series.length);
  const labelIdx = new Set(
    Array.from({ length: labelCount }, (_, i) => Math.round((i * (series.length - 1)) / Math.max(1, labelCount - 1)))
  );

  return (
    <div className="flex flex-col gap-2">
      <div className="flex h-52 items-end gap-1">
        {series.map((p, i) => {
          const h = Math.max(2, (values[i] / max) * 100);
          const label =
            metric === "invocations" ? `${formatCount(p.invocations)} calls` : formatPax(p.revenue_wei);
          return (
            <div
              key={p.date}
              className={cn(
                "flex-1 rounded-t-sm transition-opacity hover:opacity-80",
                metric === "invocations" ? "bg-primary" : "bg-[color:var(--chart-2)]"
              )}
              style={{ height: `${h}%` }}
              title={`${shortDate(p.date)} · ${label}`}
            />
          );
        })}
      </div>
      <div className="flex justify-between text-xs text-muted-foreground">
        {series.map((p, i) => (labelIdx.has(i) ? <span key={p.date}>{shortDate(p.date)}</span> : null))}
      </div>
    </div>
  );
}

export default function Analytics({ loaderData }: Route.ComponentProps) {
  const { analytics, service } = loaderData;
  const [metric, setMetric] = useState<"invocations" | "revenue">("invocations");

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-3">
        <Link
          to={`/dashboard/services/${service.id}`}
          className="inline-flex w-fit items-center gap-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
        >
          <ArrowLeft className="size-4" />
          Back to {service.display_name}
        </Link>
        <div className="flex flex-col gap-2">
          <p className="eyebrow">Analytics · last 30 days</p>
          <h1 className="text-h1 text-foreground">{service.display_name}</h1>
        </div>
      </header>

      <Surface className="grid grid-cols-2 gap-6 p-6 sm:grid-cols-3 lg:grid-cols-5">
        <Stat label="Total calls" value={formatCount(analytics.total_invocations)} />
        <Stat label="Revenue" value={formatPax(analytics.total_revenue_wei)} />
        <Stat label="Avg latency" value={`${Math.round(analytics.avg_latency_ms)} ms`} />
        <Stat label="Success rate" value={`${(analytics.success_rate * 100).toFixed(1)}%`} />
        <Stat label="Uptime" value={formatUptime(analytics.uptime_bps)} />
      </Surface>

      <Surface className="flex flex-col gap-6 p-6">
        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <h2 className="text-h3 text-foreground">Daily {metric === "invocations" ? "calls" : "revenue"}</h2>
          <AnimatedTabs
            tabs={[
              { id: "invocations", label: "Calls" },
              { id: "revenue", label: "Revenue" },
            ]}
            activeTab={metric}
            variant="segment"
            layoutId="analytics-metric"
            onChange={(id) => setMetric(id as "invocations" | "revenue")}
          />
        </div>
        <BarChart series={analytics.series} metric={metric} />
      </Surface>

      <Surface className="flex flex-col gap-4 p-6">
        <h2 className="text-h3 text-foreground">Top operations</h2>
        <div className="overflow-hidden rounded-xl bg-secondary/40">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-muted-foreground">
                <th className="px-4 py-3 font-medium">Operation</th>
                <th className="px-4 py-3 text-right font-medium">Calls</th>
                <th className="px-4 py-3 text-right font-medium">Revenue</th>
              </tr>
            </thead>
            <tbody>
              {analytics.top_operations.map((op) => (
                <tr key={op.operation} className="text-foreground">
                  <td className="px-4 py-3">
                    <code className="mono">{op.operation}</code>
                  </td>
                  <td className="px-4 py-3 text-right tabular-nums">{formatCount(op.invocations)}</td>
                  <td className="px-4 py-3 text-right tabular-nums">{formatPax(op.revenue_wei)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Surface>
    </div>
  );
}
