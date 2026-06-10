import { useState } from "react";
import { Form, Link } from "react-router";
import { Coins, Activity, Search, SlidersHorizontal } from "lucide-react";
import type { Route } from "./+types/discover";
import { getEnv } from "@/lib/env";
import { createDeusClient } from "@/lib/deus.server";
import { ServiceCard, toCardModel } from "@/components/service-card";
import { EmptyState, SectionHeading } from "@/components/ui";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import AnimatedTabs from "../../components/ui/smoothui/animated-tabs";
import AnimatedInput from "../../components/ui/smoothui/animated-input";
import Checkbox from "../../components/ui/smoothui/checkbox";
import { paxToWei } from "@/lib/format";

export function meta(_: Route.MetaArgs) {
  return [{ title: "Discover servicesDeus" }];
}

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const q = url.searchParams.get("q") ?? "";
  const kind = url.searchParams.get("kind") ?? "";
  const maxPrice = url.searchParams.get("max_price") ?? "";
  const minUptime = url.searchParams.get("min_uptime") ?? "";
  const confidential = url.searchParams.get("confidential") === "1";

  const filters: Record<string, string> = {};
  if (kind) filters.kind = kind;
  if (maxPrice) filters.max_price_wei = paxToWei(maxPrice);
  if (minUptime) filters.min_uptime_bps = String(Math.round(Number(minUptime) * 100));
  if (confidential) filters.confidential = "true";

  const deus = createDeusClient(env);
  let results: Awaited<ReturnType<typeof deus.discover>>["results"] = [];
  let failed = false;
  try {
    const r = await deus.discover({ query: q, filters, limit: 48 });
    results = r.results;
  } catch {
    failed = true;
  }
  return { q, kind, maxPrice, minUptime, confidential, results, failed };
}

const KINDS = [
  { id: "", label: "All" },
  { id: "data", label: "Data" },
  { id: "agent", label: "Agents" },
];

export default function Discover({ loaderData }: Route.ComponentProps) {
  const { results } = loaderData;
  const [q, setQ] = useState(loaderData.q);
  const [kind, setKind] = useState(loaderData.kind);
  const [maxPrice, setMaxPrice] = useState(loaderData.maxPrice);
  const [minUptime, setMinUptime] = useState(loaderData.minUptime);
  const [confidential, setConfidential] = useState(loaderData.confidential);

  return (
    <div className="mx-auto max-w-7xl px-6 py-12 sm:px-8">
      <SectionHeading
        eyebrow="Discover"
        title={loaderData.q ? `Results for “${loaderData.q}”` : "Discover services"}
        description="Search in plain language and filter by kind, price, and reliability."
      />

      {/* smoothui-controlled filters mirrored into the GET form. */}
      <Form method="get" className="mt-8 flex flex-col gap-4">
        <input type="hidden" name="q" value={q} />
        {kind ? <input type="hidden" name="kind" value={kind} /> : null}
        {maxPrice ? <input type="hidden" name="max_price" value={maxPrice} /> : null}
        {minUptime ? <input type="hidden" name="min_uptime" value={minUptime} /> : null}

        <div className="flex items-center gap-2 rounded-2xl bg-card p-2 shadow-03">
          <AnimatedInput
            className="flex-1"
            label="Describe what you need"
            value={q}
            onChange={setQ}
            icon={<Search className="size-4 text-muted-foreground" />}
            inputClassName="border-0 bg-transparent py-3 text-base"
          />
          <SmoothButton type="submit" variant="default">Search</SmoothButton>
        </div>

        <div className="flex flex-wrap items-center gap-4 rounded-xl bg-card/60 px-4 py-3">
          <span className="inline-flex items-center gap-2 text-xs text-muted-foreground">
            <SlidersHorizontal className="size-3.5" /> Filters
          </span>

          <AnimatedTabs
            tabs={KINDS}
            activeTab={kind}
            variant="segment"
            layoutId="discover-kind"
            onChange={setKind}
          />

          <AnimatedInput
            className="w-36"
            label="Max price (PAX)"
            value={maxPrice}
            onChange={setMaxPrice}
            icon={<Coins className="size-3.5 text-muted-foreground" />}
          />

          <AnimatedInput
            className="w-36"
            label="Min uptime (%)"
            value={minUptime}
            onChange={setMinUptime}
            icon={<Activity className="size-3.5 text-muted-foreground" />}
          />

          <label className="inline-flex cursor-pointer items-center gap-2 text-sm text-muted-foreground">
            <Checkbox
              checked={confidential}
              onCheckedChange={setConfidential}
              name="confidential"
              value="1"
            />
            Confidential only
          </label>

          <SmoothButton type="submit" variant="secondary" size="sm" className="ml-auto">
            Apply filters
          </SmoothButton>
        </div>
      </Form>

      <p className="mt-8 body-sm text-muted-foreground">
        {results.length} {results.length === 1 ? "service" : "services"} found
      </p>

      {results.length > 0 ? (
        <div className="mt-4 grid grid-cols-1 gap-5 sm:grid-cols-2 lg:grid-cols-3">
          {results.map((r) => (
            <ServiceCard key={r.id} service={toCardModel(r)} />
          ))}
        </div>
      ) : (
        <EmptyState
          className="mt-4"
          icon={<Search className="size-6" />}
          title="No services match"
          description="Try a broader search or relax your filters."
          action={
            <Link to="/discover">
              <SmoothButton variant="secondary" size="sm">Clear search</SmoothButton>
            </Link>
          }
        />
      )}
    </div>
  );
}
