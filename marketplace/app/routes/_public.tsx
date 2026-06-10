import { Outlet } from "react-router";
import type { Route } from "./+types/_public";
import { getEnv } from "@/lib/env";
import { getUser, getWallet, isDevAuth } from "@/lib/auth.server";
import { SiteHeader } from "@/components/site-header";
import { SiteFooter } from "@/components/site-footer";

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const [user, wallet] = await Promise.all([
    getUser(request, env),
    getWallet(request, env),
  ]);
  return { user, wallet, allowDev: isDevAuth(env) };
}

export default function PublicLayout({ loaderData }: Route.ComponentProps) {
  const { user, wallet, allowDev } = loaderData;
  return (
    <div className="flex min-h-dvh flex-col bg-background">
      <SiteHeader user={user} wallet={wallet} allowDev={allowDev} />
      <main className="flex-1">
        <Outlet />
      </main>
      <SiteFooter />
    </div>
  );
}
