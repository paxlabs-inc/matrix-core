import { Form } from "react-router";
import { Fingerprint, LogOut, Mail, Wallet } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import type { Route } from "./+types/account";
import { getEnv } from "@/lib/env";
import {
  callerIdentityFor,
  developerIdentityFor,
  getWallet,
  isDevAuth,
  requireUser,
} from "@/lib/auth.server";
import { createDeusClient } from "@/lib/deus.server";
import { CopyChip, EmptyState, Stat, Surface } from "@/components/ui";
import { CountFlow, PaxFlow } from "@/components/pax";
import { WalletButton } from "@/components/wallet";

export function meta() {
  return [{ title: "Account · Deus" }];
}

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const user = await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, {
    developer: developerIdentityFor(wallet),
    caller: callerIdentityFor(user, wallet),
  });
  const [me, spend] = await Promise.all([deus.me(), deus.spend()]);
  return { me, spend, user, wallet, allowDev: isDevAuth(env) };
}

function InfoRow({
  icon,
  label,
  value,
  copy,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  copy?: boolean;
}) {
  return (
    <div className="flex items-center gap-3 rounded-xl bg-secondary/50 px-4 py-3 shadow-01">
      <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-background text-muted-foreground">
        {icon}
      </span>
      <div className="min-w-0 flex-1">
        <p className="eyebrow">{label}</p>
        <p className="mono truncate text-sm text-foreground">{value}</p>
      </div>
      {copy ? <CopyChip value={value} /> : null}
    </div>
  );
}

export default function Account({ loaderData }: Route.ComponentProps) {
  const { me, spend, user, wallet, allowDev } = loaderData;
  const displayName = me.display_name || user.displayName || "Developer";
  const email = me.email || user.email;
  const did = me.did || user.did;

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <p className="eyebrow">Settings</p>
        <h1 className="text-h1 text-foreground">Account</h1>
        <p className="body-sm text-muted-foreground">Your identity, wallet, and consumption on the network.</p>
      </header>

      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <Surface className="flex flex-col gap-4 p-6">
          <div className="flex items-center gap-3">
            <span className="flex size-12 items-center justify-center rounded-full bg-primary/15 text-lg font-medium text-[color:var(--accent-fore)]">
              {displayName.charAt(0).toUpperCase()}
            </span>
            <div className="min-w-0">
              <p className="text-h3 truncate text-foreground">{displayName}</p>
              {user.provider ? <p className="text-xs text-muted-foreground">via {user.provider}</p> : null}
            </div>
          </div>
          <div className="flex flex-col gap-2">
            {email ? <InfoRow icon={<Mail className="size-4" />} label="Email" value={email} /> : null}
            <InfoRow icon={<Fingerprint className="size-4" />} label="Decentralized ID" value={did} copy />
          </div>
        </Surface>

        <Surface className="flex flex-col gap-4 p-6">
          <div className="flex items-center gap-2">
            <Wallet className="size-4 text-muted-foreground" />
            <h2 className="text-h3 text-foreground">Wallet</h2>
          </div>
          <p className="body-sm text-muted-foreground">
            Your wallet receives payouts and signs the receipts that confirm each paid call. Connect it to settle
            earnings directly.
          </p>
          <WalletButton wallet={wallet} allowDev={allowDev} />
        </Surface>
      </div>

      <Surface className="flex flex-col gap-4 p-6">
        <div className="flex items-center justify-between">
          <h2 className="text-h3 text-foreground">Your spend</h2>
          <Stat label="Total spent" value={<PaxFlow wei={spend.total_spent_wei} />} className="items-end text-right" />
        </div>
        {spend.entries.length === 0 ? (
          <EmptyState
            title="Nothing spent yet"
            description="When you call services on the network, your usage shows up here."
          />
        ) : (
          <div className="overflow-x-auto rounded-xl bg-secondary/40">
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-muted-foreground">
                  <th className="px-4 py-3 font-medium">Service</th>
                  <th className="px-4 py-3 text-right font-medium">Calls</th>
                  <th className="px-4 py-3 text-right font-medium">Spent</th>
                </tr>
              </thead>
              <tbody>
                {spend.entries.map((e) => (
                  <tr key={e.service_id} className="text-foreground">
                    <td className="px-4 py-3">{e.display_name}</td>
                    <td className="px-4 py-3 text-right tabular-nums"><CountFlow value={e.invocations} /></td>
                    <td className="px-4 py-3 text-right tabular-nums"><PaxFlow wei={e.total_wei} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Surface>

      <div>
        <Form method="post" action="/logout">
          <SmoothButton type="submit" variant="outline">
            <LogOut className="size-4" />
            Sign out
          </SmoothButton>
        </Form>
      </div>
    </div>
  );
}
