import { Outlet, useLoaderData } from "react-router";
import type { Route } from "./+types/_layout";
import { getEnv } from "@/lib/env";
import { getWallet, isDevAuth, requireUser } from "@/lib/auth.server";
import type { AppUser } from "@/lib/auth.server";
import { DashboardNav } from "@/components/dashboard-nav";

/** Shared context handed to every dashboard child via `useOutletContext`. */
export interface DashboardOutletContext {
  user: AppUser;
  wallet: string | null;
  allowDev: boolean;
}

export function meta() {
  return [{ title: "Dashboard · Deus" }];
}

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const user = await requireUser(request, env);
  const wallet = await getWallet(request, env);
  return { user, wallet, allowDev: isDevAuth(env) };
}

export default function DashboardLayout() {
  const { user, wallet, allowDev } = useLoaderData<typeof loader>();
  const context: DashboardOutletContext = { user, wallet, allowDev };

  return (
    <div className="min-h-screen bg-background lg:flex">
      <DashboardNav user={user} wallet={wallet} allowDev={allowDev} />
      <main className="min-w-0 flex-1">
        <div className="mx-auto w-full max-w-6xl px-6 py-8 lg:px-8 lg:py-12">
          <Outlet context={context} />
        </div>
      </main>
    </div>
  );
}
