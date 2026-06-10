import { Link, redirect } from "react-router";
import type { Route } from "./+types/auth.callback";
import { getEnv } from "@/lib/env";
import {
  clearPkceVerifier,
  commitSession,
  exchangeOAuthCode,
  getSession,
  readPkceVerifier,
  rotateSession,
} from "@/lib/auth.server";

/** Auth exchange — never cacheable. */
export function headers() {
  return { "Cache-Control": "private, no-store" };
}

function safeNext(value: string | null | undefined): string {
  if (value && value.startsWith("/") && !value.startsWith("//")) return value;
  return "/dashboard";
}

/**
 * PKCE callback: GoTrue redirects here with ?code=. The exchange happens
 * entirely server-side (code + cookie-held verifier); tokens never appear in
 * a URL or in client JS. On success the session is rotated and committed.
 */
export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const next = safeNext(url.searchParams.get("next"));

  const code = url.searchParams.get("code");
  if (!code) {
    return { error: "No sign-in details were found. Please try again." };
  }
  const verifier = await readPkceVerifier(env, request);
  if (!verifier) {
    return { error: "Your sign-in attempt expired. Please try again." };
  }

  const user = await exchangeOAuthCode(env, code, verifier);
  if (!user) {
    return { error: "We couldn't verify your sign-in. Please try again." };
  }

  const session = await rotateSession(env, await getSession(request, env));
  session.set("user", JSON.stringify(user));
  const headers = new Headers();
  headers.append("Set-Cookie", await commitSession(env, session));
  headers.append("Set-Cookie", await clearPkceVerifier(env));
  throw redirect(next, { headers });
}

export default function AuthCallback({ loaderData }: Route.ComponentProps) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-6">
      <div className="flex w-full max-w-sm flex-col items-center gap-5 rounded-2xl bg-card p-8 text-center shadow-04">
        <span className="flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-02">
          <span className="font-display text-2xl leading-none">D</span>
        </span>
        <div className="flex flex-col gap-2">
          <h1 className="text-h3 text-foreground">Sign-in failed</h1>
          <p className="body-sm text-muted-foreground">{loaderData.error}</p>
        </div>
        <Link
          to="/login"
          className="text-sm font-medium text-primary underline-offset-4 hover:underline"
        >
          Back to sign in
        </Link>
      </div>
    </div>
  );
}
