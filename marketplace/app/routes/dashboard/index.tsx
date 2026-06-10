import { Link } from "react-router";
import { Boxes, Plus } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import type { Route } from "./+types/index";
import { getEnv } from "@/lib/env";
import { callerIdentityFor, developerIdentityFor, getWallet, requireUser } from "@/lib/auth.server";
import { createDeusClient } from "@/lib/deus.server";
import type { MyService } from "@/lib/deus.types";
import { formatUptime } from "@/lib/format";
import { EmptyState, KindBadge, Stat, StatusBadge, Surface } from "@/components/ui";
import { CountFlow, PaxFlow } from "@/components/pax";

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const user = await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, {
    developer: developerIdentityFor(wallet),
    caller: callerIdentityFor(user, wallet),
  });
  const services = await deus.myServices();
  return { services };
}

function sumRevenue(services: MyService[]): string {
  let total = 0n;
  for (const s of services) {
    try {
      total += BigInt(s.revenue_wei || "0");
    } catch {
      // ignore malformed wei
    }
  }
  return total.toString();
}

function ServiceCard({ service }: { service: MyService }) {
  return (
    <Surface className="flex flex-col gap-5 p-5 transition-transform duration-300 hover:-translate-y-0.5">
      <div className="flex items-start justify-between gap-3">
        <div className="flex flex-col gap-2">
          <h3 className="text-h3 text-foreground">{service.display_name}</h3>
          <code className="mono text-xs text-muted-foreground">{service.slug}</code>
        </div>
        <StatusBadge status={service.status} />
      </div>

      <div className="grid grid-cols-3 gap-4 rounded-lg bg-secondary/50 p-4">
        <Stat label="Calls" value={<CountFlow value={service.invocations} />} />
        <Stat label="Revenue" value={<PaxFlow wei={service.revenue_wei} withSymbol={false} />} hint="PAX" />
        <Stat label="Uptime" value={formatUptime(service.uptime_bps)} />
      </div>

      <div className="flex items-center justify-between gap-3">
        <KindBadge kind={service.kind} />
        <div className="flex items-center gap-2">
          <SmoothButton asChild variant="ghost" size="sm">
            <Link to={`/dashboard/services/${service.id}/analytics`}>Analytics</Link>
          </SmoothButton>
          <SmoothButton asChild variant="secondary" size="sm">
            <Link to={`/dashboard/services/${service.id}`}>Manage</Link>
          </SmoothButton>
        </div>
      </div>
    </Surface>
  );
}

export default function DashboardIndex({ loaderData }: Route.ComponentProps) {
  const { services } = loaderData;
  const totalInvocations = services.reduce((acc, s) => acc + (s.invocations || 0), 0);
  const totalRevenueWei = sumRevenue(services);

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div className="flex flex-col gap-2">
          <p className="eyebrow">Developer</p>
          <h1 className="text-h1 text-foreground">Your services</h1>
          <p className="body-sm text-muted-foreground">
            Listings you own across the Deus networkusage, revenue, and health at a glance.
          </p>
        </div>
        <SmoothButton asChild size="lg">
          <Link to="/dashboard/services/new">
            <Plus className="size-4" />
            New listing
          </Link>
        </SmoothButton>
      </header>

      {services.length > 0 ? (
        <>
          <Surface className="grid grid-cols-1 gap-6 p-6 sm:grid-cols-3">
            <Stat label="Active services" value={<CountFlow value={services.length} />} />
            <Stat label="Total calls" value={<CountFlow value={totalInvocations} />} hint="across all listings" />
            <Stat label="Total revenue" value={<PaxFlow wei={totalRevenueWei} />} hint="settled in PAX" />
          </Surface>

          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            {services.map((service) => (
              <ServiceCard key={service.id} service={service} />
            ))}
          </div>
        </>
      ) : (
        <EmptyState
          icon={<Boxes className="size-6" />}
          title="No services yet"
          description="Publish your first data or agent service to start earning on the Deus network."
          action={
            <SmoothButton asChild>
              <Link to="/dashboard/services/new">
                <Plus className="size-4" />
                New listing
              </Link>
            </SmoothButton>
          }
        />
      )}
    </div>
  );
}
