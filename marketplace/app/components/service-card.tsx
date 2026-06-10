import { Link } from "react-router";
import { ArrowUpRight, Gauge, Activity } from "lucide-react";
import { Badge, KindBadge } from "@/components/ui";
import { PaxFlow } from "@/components/pax";
import { cn } from "@/lib/utils";
import { formatQuality, formatUptime } from "@/lib/format";
import type { CatalogItem, DiscoverResult } from "@/lib/deus.types";

export interface ServiceCardModel {
  id: string;
  slug: string;
  display_name: string;
  summary: string;
  kind: string;
  quality_score?: string;
  uptime_bps?: number;
  price_wei?: string;
  unit?: string;
  tags?: string[];
}

export function toCardModel(r: DiscoverResult | CatalogItem): ServiceCardModel {
  const op = "operations" in r ? r.operations[0] : undefined;
  return {
    id: r.id,
    slug: r.slug,
    display_name: r.display_name,
    summary: r.summary,
    kind: r.kind,
    quality_score: r.quality_score,
    uptime_bps: r.uptime_bps,
    price_wei: "price_wei" in r ? r.price_wei : op?.price_wei,
    unit: "unit" in r ? r.unit : op?.unit,
    tags: "tags" in r ? r.tags : undefined,
  };
}

export function ServiceCard({
  service,
  className,
}: {
  service: ServiceCardModel;
  className?: string;
}) {
  return (
    <Link
      to={`/services/${service.slug}`}
      className={cn(
        "group hover-lift relative flex flex-col gap-4 rounded-xl bg-card p-5 shadow-02 transition-shadow hover:shadow-04",
        className
      )}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="flex flex-col gap-2">
          <KindBadge kind={service.kind} />
          <h3 className="text-h3 leading-tight text-foreground">{service.display_name}</h3>
        </div>
        <ArrowUpRight className="size-5 shrink-0 text-muted-foreground transition-colors group-hover:text-foreground" />
      </div>

      <p className="body-sm line-clamp-2 text-muted-foreground">{service.summary}</p>

      {service.tags && service.tags.length > 0 ? (
        <div className="flex flex-wrap gap-2">
          {service.tags.slice(0, 3).map((t) => (
            <span key={t} className="rounded-full bg-secondary px-2 py-1 text-xs text-muted-foreground">
              {t}
            </span>
          ))}
        </div>
      ) : null}

      <div className="mt-auto flex items-center justify-between gap-3 pt-1">
        <div className="flex items-center gap-3 text-xs text-muted-foreground">
          {service.quality_score ? (
            <span className="inline-flex items-center gap-1">
              <Gauge className="size-3.5" />
              {formatQuality(service.quality_score)} quality
            </span>
          ) : null}
          {service.uptime_bps ? (
            <span className="inline-flex items-center gap-1">
              <Activity className="size-3.5" />
              {formatUptime(service.uptime_bps)}
            </span>
          ) : null}
        </div>
        {service.price_wei ? (
          <Badge tone="accent">
            <PaxFlow wei={service.price_wei} />
            {service.unit ? <span className="opacity-70">/ {service.unit}</span> : null}
          </Badge>
        ) : null}
      </div>
    </Link>
  );
}
