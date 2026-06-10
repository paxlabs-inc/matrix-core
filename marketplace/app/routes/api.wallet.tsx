import { redirect } from "react-router";
import type { Route } from "./+types/api.wallet";
import { getEnv } from "@/lib/env";
import { commitSession, getSession } from "@/lib/auth.server";

/** Resource route: link/unlink the caller's EVM wallet to the session. */
export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const session = await getSession(request, env);
  const form = await request.formData();
  const intent = form.get("intent");

  if (intent === "link") {
    const address = String(form.get("address") ?? "").trim().toLowerCase();
    if (/^0x[0-9a-f]{40}$/.test(address)) {
      session.set("wallet", address);
    }
  } else if (intent === "unlink") {
    session.unset("wallet");
  }

  const referer = request.headers.get("Referer");
  return redirect(referer ?? "/", {
    headers: { "Set-Cookie": await commitSession(env, session) },
  });
}

export function loader() {
  return redirect("/");
}
