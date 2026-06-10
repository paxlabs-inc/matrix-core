import { data, redirect } from "react-router";
import type { Route } from "./+types/api.wallet";
import { getEnv } from "@/lib/env";
import { commitSession, getSession, isDevAuth } from "@/lib/auth.server";
import { createDeusClient, DeusApiError } from "@/lib/deus.server";
import { allowRequest, clientKey } from "@/lib/limits.server";
import { buildSiweMessage } from "@/lib/siwe.server";
import { evmAddressSchema, firstIssue, siweLinkSchema } from "@/lib/validate.server";

/**
 * Resource route: link/unlink the caller's EVM wallet via SIWE (EIP-4361).
 *
 * Flow: `prepare` fetches a deusd nonce and returns the message to sign;
 * the client signs it with `personal_sign`; `link` forwards
 * {message, signature} to deusd, which recovers the signer and mints a
 * developer token. Typing in someone else's address proves nothing and
 * links nothing.
 */
export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);

  if (!(await allowRequest(env.RL_WALLET, clientKey(request)))) {
    return data({ error: "Too many wallet requests. Try again in a minute." }, { status: 429 });
  }

  const session = await getSession(request, env);
  const form = await request.formData();
  const intent = form.get("intent");
  const deus = createDeusClient(env);

  if (intent === "prepare") {
    const parsed = evmAddressSchema.safeParse(String(form.get("address") ?? ""));
    if (!parsed.success) {
      return data({ error: "Invalid wallet address." }, { status: 400 });
    }
    const address = parsed.data;
    try {
      const { nonce, expires_at } = await deus.developerNonce();
      const url = new URL(request.url);
      const message = buildSiweMessage({
        domain: url.host,
        origin: url.origin,
        address,
        nonce,
        expirationTime: expires_at,
      });
      return data({ message });
    } catch (err) {
      return data(
        { error: walletErrorMessage(err, "Wallet linking is unavailable right now.") },
        { status: 502 }
      );
    }
  }

  if (intent === "link") {
    const parsed = siweLinkSchema.safeParse({
      message: String(form.get("message") ?? ""),
      signature: String(form.get("signature") ?? ""),
    });
    if (!parsed.success) {
      return data({ error: firstIssue(parsed.error) }, { status: 400 });
    }
    try {
      const res = await deus.developerAuth(parsed.data.message, parsed.data.signature);
      session.set("wallet", res.wallet.toLowerCase());
      session.set("developerToken", res.token);
      return data(
        { ok: true, wallet: res.wallet },
        { headers: { "Set-Cookie": await commitSession(env, session) } }
      );
    } catch (err) {
      return data(
        { error: walletErrorMessage(err, "We couldn't verify that signature.") },
        { status: 401 }
      );
    }
  }

  // Local-dev convenience only: link a fake wallet without a signature so the
  // dashboard is usable against the mock backend. Gated twice (dev auth flag
  // AND non-production) and never mints a developer token.
  if (intent === "dev-link" && isDevAuth(env)) {
    const parsed = evmAddressSchema.safeParse(String(form.get("address") ?? ""));
    const address = parsed.success ? parsed.data.toLowerCase() : "";
    if (address) {
      session.set("wallet", address);
      session.unset("developerToken");
    }
    return data(
      { ok: true, wallet: address },
      { headers: { "Set-Cookie": await commitSession(env, session) } }
    );
  }

  if (intent === "unlink") {
    session.unset("wallet");
    session.unset("developerToken");
    return data(
      { ok: true },
      { headers: { "Set-Cookie": await commitSession(env, session) } }
    );
  }

  return data({ error: "Unknown intent." }, { status: 400 });
}

export function loader() {
  return redirect("/");
}

function walletErrorMessage(err: unknown, fallback: string): string {
  if (err instanceof DeusApiError && err.status === 401) {
    return "Signature verification failed. Please try connecting again.";
  }
  return fallback;
}
