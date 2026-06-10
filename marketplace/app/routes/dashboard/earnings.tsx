import { Form } from "react-router";
import { ArrowDownToLine } from "lucide-react";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import type { Route } from "./+types/earnings";
import { getEnv } from "@/lib/env";
import {
  callerIdentityFor,
  developerIdentityFor,
  getWallet,
  requireUser,
} from "@/lib/auth.server";
import { createDeusClient, DeusApiError } from "@/lib/deus.server";
import { formatDate, shortAddress } from "@/lib/format";
import { CopyChip, EmptyState, StatusBadge, Surface } from "@/components/ui";
import { ActionToast } from "@/components/feedback";
import { PaxFlow } from "@/components/pax";

export function meta() {
  return [{ title: "Earnings · Deus" }];
}

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, { developer: developerIdentityFor(wallet) });
  const earnings = await deus.earnings();
  return { earnings };
}

export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const user = await requireUser(request, env);
  const wallet = await getWallet(request, env);
  const deus = createDeusClient(env, {
    developer: developerIdentityFor(wallet),
    caller: callerIdentityFor(user, wallet),
  });
  const form = await request.formData();
  const payoutAddress = String(form.get("payout_address") ?? "").trim() || wallet || "";

  if (!payoutAddress) return { error: "Connect a wallet to receive payouts." };
  try {
    const mine = await deus.myServices();
    if (mine.length === 0) return { error: "You have no services to settle yet." };
    const res = await deus.payout(mine[0].id, payoutAddress);
    return { ok: true as const, settlementId: res.settlement_id };
  } catch (err) {
    if (err instanceof DeusApiError) return { error: err.message };
    return { error: "Payout request failed. Please try again." };
  }
}

export default function Earnings({ loaderData, actionData }: Route.ComponentProps) {
  const { earnings } = loaderData;

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <p className="eyebrow">Developer</p>
        <h1 className="text-h1 text-foreground">Earnings</h1>
        <p className="body-sm text-muted-foreground">Revenue settled to you in PAX across all your services.</p>
      </header>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <Surface className="flex flex-col gap-1 p-6 shadow-04 ring-1 ring-[color:var(--accent-tint)]">
          <span className="eyebrow">Available</span>
          <span className="text-h2 text-foreground"><PaxFlow wei={earnings.available_wei} /></span>
          <span className="body-sm text-muted-foreground">ready to withdraw</span>
        </Surface>
        <Surface className="flex flex-col gap-1 p-6">
          <span className="eyebrow">Pending</span>
          <span className="text-h2 text-foreground"><PaxFlow wei={earnings.pending_wei} /></span>
          <span className="body-sm text-muted-foreground">settling this window</span>
        </Surface>
        <Surface className="flex flex-col gap-1 p-6">
          <span className="eyebrow">Total earned</span>
          <span className="text-h2 text-foreground"><PaxFlow wei={earnings.total_earned_wei} /></span>
          <span className="body-sm text-muted-foreground">all time</span>
        </Surface>
      </div>

      <Surface className="flex flex-col gap-5 p-6">
        <div className="flex flex-col gap-1">
          <h2 className="text-h3 text-foreground">Withdraw to wallet</h2>
          <p className="body-sm text-muted-foreground">Send your available balance to your payout address.</p>
        </div>
        <div className="flex flex-col gap-4 rounded-xl bg-secondary/50 p-5 shadow-01 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex flex-col gap-1">
            <span className="eyebrow">Payout address</span>
            {earnings.payout_address ? (
              <span className="flex items-center gap-2">
                <code className="mono text-sm text-foreground">{shortAddress(earnings.payout_address, 6)}</code>
                <CopyChip value={earnings.payout_address} />
              </span>
            ) : (
              <span className="body-sm text-muted-foreground">Connect a wallet to set one.</span>
            )}
          </div>
          <Form method="post">
            {earnings.payout_address ? (
              <input type="hidden" name="payout_address" value={earnings.payout_address} />
            ) : null}
            <SmoothButton type="submit">
              <ArrowDownToLine className="size-4" />
              Request payout
            </SmoothButton>
          </Form>
        </div>
        <ActionToast
          message={
            actionData?.ok
              ? `Payout requestedsettlement ${shortAddress(actionData.settlementId, 5)} is on its way.`
              : null
          }
          type="success"
        />
        <ActionToast message={actionData?.error} type="error" />
      </Surface>

      <Surface className="flex flex-col gap-4 p-6">
        <h2 className="text-h3 text-foreground">Settlements</h2>
        {earnings.settlements.length === 0 ? (
          <EmptyState title="No settlements yet" description="Earnings appear here once your services are used." />
        ) : (
          <div className="overflow-x-auto rounded-xl bg-secondary/40">
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-muted-foreground">
                  <th className="px-4 py-3 font-medium">Window</th>
                  <th className="px-4 py-3 text-right font-medium">Amount</th>
                  <th className="px-4 py-3 font-medium">Status</th>
                  <th className="px-4 py-3 font-medium">Transaction</th>
                </tr>
              </thead>
              <tbody>
                {earnings.settlements.map((s) => (
                  <tr key={s.id} className="text-foreground">
                    <td className="whitespace-nowrap px-4 py-3 text-muted-foreground">
                      {formatDate(s.window_start)} – {formatDate(s.window_end)}
                    </td>
                    <td className="px-4 py-3 text-right tabular-nums">{<PaxFlow wei={s.amount_wei} />}</td>
                    <td className="px-4 py-3">
                      <StatusBadge status={s.status} />
                    </td>
                    <td className="px-4 py-3">
                      {s.tx_hash ? (
                        <span className="flex items-center gap-2">
                          <code className="mono text-xs text-muted-foreground">{shortAddress(s.tx_hash, 6)}</code>
                          <CopyChip value={s.tx_hash} />
                        </span>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Surface>
    </div>
  );
}
