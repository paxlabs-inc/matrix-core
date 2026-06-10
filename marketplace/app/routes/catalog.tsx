import { Link, useNavigate, useSearchParams } from "react-router";
import { Boxes } from "lucide-react";
import type { Route } from "./+types/catalog";
import { getEnv } from "@/lib/env";
import { createDeusClient } from "@/lib/deus.server";
import { ServiceCard, toCardModel } from "@/components/service-card";
import { EmptyState, SectionHeading } from "@/components/ui";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import Pagination from "../../components/ui/smoothui/pagination";
import AnimatedTabs from "../../components/ui/smoothui/animated-tabs";

export function meta(_: Route.MetaArgs) {
  return [{ title: "CatalogDeus" }];
}

const PAGE_SIZE = 12;

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const page = Math.max(1, Number(url.searchParams.get("page") ?? 1) || 1);
  const kind = url.searchParams.get("kind") ?? "";
  const deus = createDeusClient(env);
  try {
    // Real Go endpoint paginates by limit/offset ({services,total,limit,offset}).
    const cat = await deus.catalog({
      limit: PAGE_SIZE,
      offset: (page - 1) * PAGE_SIZE,
      kind: kind || undefined,
    });
    return { items: cat.services, total: cat.total, page, kind };
  } catch {
    return { items: [], total: 0, page, kind };
  }
}

const KINDS = [
  { id: "", label: "All" },
  { id: "data", label: "Data" },
  { id: "agent", label: "Agents" },
];

export default function Catalog({ loaderData }: Route.ComponentProps) {
  const { items, total, page, kind } = loaderData;
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  function goToPage(p: number) {
    const next = new URLSearchParams(params);
    next.set("page", String(p));
    setParams(next);
    if (typeof window !== "undefined") window.scrollTo({ top: 0, behavior: "smooth" });
  }

  function setKind(value: string) {
    const next = new URLSearchParams(params);
    if (value) next.set("kind", value);
    else next.delete("kind");
    next.delete("page");
    navigate(`/catalog?${next.toString()}`);
  }

  return (
    <div className="mx-auto max-w-7xl px-6 py-12 sm:px-8">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <SectionHeading
          eyebrow="Catalog"
          title="Browse every service"
          description={`${total} live services across data feeds and autonomous agents.`}
        />
        <AnimatedTabs
          tabs={KINDS}
          activeTab={kind}
          variant="pill"
          layoutId="catalog-kind"
          onChange={setKind}
        />
      </div>

      {items.length > 0 ? (
        <>
          <div className="mt-8 grid grid-cols-1 gap-5 sm:grid-cols-2 lg:grid-cols-3">
            {items.map((s) => (
              <ServiceCard key={s.id} service={toCardModel(s)} />
            ))}
          </div>
          {totalPages > 1 ? (
            <div className="mt-12">
              <Pagination page={page} totalPages={totalPages} onPageChange={goToPage} />
            </div>
          ) : null}
        </>
      ) : (
        <EmptyState
          className="mt-8"
          icon={<Boxes className="size-6" />}
          title="Nothing here yet"
          description="No services match this filter."
          action={
            <Link to="/catalog">
              <SmoothButton variant="secondary" size="sm">Reset</SmoothButton>
            </Link>
          }
        />
      )}
    </div>
  );
}
