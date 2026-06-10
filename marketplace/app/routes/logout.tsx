import { redirect } from "react-router";
import type { Route } from "./+types/logout";
import { getEnv } from "@/lib/env";
import { destroySession, getSession } from "@/lib/auth.server";

export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const session = await getSession(request, env);
  return redirect("/", {
    headers: { "Set-Cookie": await destroySession(env, session) },
  });
}

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const session = await getSession(request, env);
  return redirect("/", {
    headers: { "Set-Cookie": await destroySession(env, session) },
  });
}
