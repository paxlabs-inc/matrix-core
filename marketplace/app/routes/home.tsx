import { Link } from "react-router";
import { Search, Sparkles, Boxes, Wallet, ArrowRight } from "lucide-react";
import type { Route } from "./+types/home";
import { getEnv } from "@/lib/env";
import { createDeusClient } from "@/lib/deus.server";
import { ServiceCard, toCardModel } from "@/components/service-card";
import { SectionHeading, Surface } from "@/components/ui";
import { SearchDock } from "@/components/search-dock";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import { StatsGrid } from "../../components/ui/smoothui/stats-1";
import { formatCount } from "@/lib/format";

export function meta(_: Route.MetaArgs) {
  return [
    { title: "Deusthe marketplace for data & agent services" },
    {
      name: "description",
      content:
        "Discover, try, and call data and agent services in plain language. Paid per use, settled natively in PAX on Paxeer.",
    },
  ];
}

const EXAMPLES = ["weather forecast", "translate text", "image recognition", "market data"];

export async function loader({ context }: Route.LoaderArgs) {
  const deus = createDeusClient(getEnv(context));
  try {
    const cat = await deus.catalog({ limit: 24 });
    const featured = cat.services.slice(0, 6).map(toCardModel);
    const dataCount = cat.services.filter((s) => s.kind === "data").length;
    const agentCount = cat.services.filter((s) => s.kind === "agent").length;
    return { featured, total: cat.total, dataCount, agentCount };
  } catch {
    return { featured: [], total: 0, dataCount: 0, agentCount: 0 };
  }
}

export default function Home({ loaderData }: Route.ComponentProps) {
  const { featured, total, dataCount, agentCount } = loaderData;
  return (
    <>
      {/* Hero */}
      <section className="relative overflow-hidden">
        <div className="pointer-events-none absolute inset-0 [background:radial-gradient(60%_50%_at_50%_-10%,color-mix(in_oklab,var(--accent-ink)_22%,transparent),transparent)]" />
        <div className="mx-auto flex max-w-4xl flex-col items-center gap-8 px-6 pt-24 pb-16 text-center sm:px-8">
          <span className="inline-flex items-center gap-2 rounded-full bg-secondary px-4 py-2 text-xs text-muted-foreground">
          </span>
          <h1 className="display-1 max-w-3xl text-balance text-foreground">
            Call any service. Pay only for what you use.
          </h1>
          <p className="body-lg max-w-2xl text-balance text-muted-foreground">
            Deus finds the right data
            or agent service, shows a price up front, and runs it settled
            natively, no subscriptions.
          </p>

          {/* Plain-language searchsmoothui ai-input morph dock. */}
          <SearchDock className="pt-10" />

          <div className="flex flex-wrap items-center justify-center gap-2">
            <span className="text-xs text-muted-foreground">Try</span>
            {EXAMPLES.map((ex) => (
              <Link
                key={ex}
                to={`/discover?q=${encodeURIComponent(ex)}`}
                className="rounded-full bg-secondary px-3 py-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
              >
                {ex}
              </Link>
            ))}
          </div>
        </div>
      </section>

      {/* Stats bandsmoothui stats-1 */}
      <StatsGrid
        title="A live network of priced services"
        description="Every listing is quoted up front and settled per call in PAX."
        stats={[
          { value: formatCount(total), label: "Live services", description: "On the network" },
          { value: formatCount(dataCount), label: "Data feeds", description: "Oracles & APIs" },
          { value: formatCount(agentCount), label: "Agents", description: "Autonomous services" },
          { value: "PAX", label: "Settlement", description: "Native, per call" },
        ]}
      />

      {/* Featured */}
      <section className="mx-auto max-w-7xl px-6 pb-16 sm:px-8">
        <div className="flex items-end justify-between gap-4">
          <SectionHeading
            eyebrow="Featured"
            title="Top services right now"
            description="Hand-picked, high-quality services ready to call."
          />
          <Link to="/catalog" className="hidden md:block">
            <SmoothButton variant="ghost" size="sm">
              Browse all
              <ArrowRight className="size-4" />
            </SmoothButton>
          </Link>
        </div>
        <div className="mt-8 grid grid-cols-1 gap-5 sm:grid-cols-2 lg:grid-cols-3">
          {featured.map((s) => (
            <ServiceCard key={s.id} service={s} />
          ))}
        </div>
      </section>

      {/* Value props */}
      <section className="mx-auto max-w-7xl px-6 pb-16 sm:px-8">
        <div className="grid grid-cols-1 gap-5 md:grid-cols-3">
          {[
            {
              icon: <Search className="size-5" />,
              title: "Search in plain language",
              body: "Describe the outcome you want. Deus extracts constraintsprice, quality, kindand ranks the best matches.",
            },
            {
              icon: <Sparkles className="size-5" />,
              title: "Try before you wire it up",
              body: "See a real price quote and run a live call right in your browser. No integration required to evaluate.",
            },
            {
              icon: <Boxes className="size-5" />,
              title: "Ship your own service",
              body: "List a proxy to your API or upload your code and have it run in the cloudbilled per call, paid out to your wallet.",
            },
          ].map((v) => (
            <Surface key={v.title} className="flex flex-col gap-3 p-6">
              <span className="flex size-11 items-center justify-center rounded-xl bg-secondary text-[color:var(--accent-fore)]">
                {v.icon}
              </span>
              <h3 className="text-h3 text-foreground">{v.title}</h3>
              <p className="body-sm text-muted-foreground">{v.body}</p>
            </Surface>
          ))}
        </div>
      </section>

      {/* CTAthree layers: tonal card, translucent accent wash, content. */}
      <section className="mx-auto max-w-7xl px-6 pt-8 pb-24 sm:px-8">
        <Surface className="relative overflow-hidden p-12">
          <div className="pointer-events-none absolute inset-0 [background:radial-gradient(70%_120%_at_85%_0%,color-mix(in_oklab,var(--accent-ink)_14%,transparent),transparent)]" />
          <div className="pointer-events-none absolute inset-x-0 bottom-0 h-24 bg-secondary/30" />
          <div className="relative flex flex-col items-start justify-between gap-8 md:flex-row md:items-center">
            <div className="flex flex-col gap-3">
              <p className="eyebrow">For builders</p>
              <h2 className="text-h2 font-medium text-foreground">
                Have an API or agent? Earn from it.
              </h2>
              <p className="body max-w-xl text-muted-foreground">
                List it on Deus in minutes. Set your price, keep your keys
                server-side, and get paid per call.
              </p>
            </div>
            <Link to="/dashboard/services/new" className="shrink-0">
              <SmoothButton variant="default" size="lg">
                <Wallet className="size-4" />
                List a service
              </SmoothButton>
            </Link>
          </div>
        </Surface>
      </section>
    </>
  );
}
