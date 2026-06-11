import { useEffect, useState } from "react";
import { Link, redirect, useSubmit } from "react-router";
import type { Route } from "./+types/auth.callback";
import { getEnv } from "@/lib/env";
import {
  clearPkceVerifier,
  commitSession,
  exchangeOAuthCode,
  getSession,
  readPkceVerifier,
  rotateSession,
  verifyAccessToken,
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
 * OAuth callback. Two return shapes are handled:
 *
 * - Implicit flow (the GoTrue deployment all Paxeer apps sign in through):
 *   tokens arrive in the URL fragment, which never reaches the server. The
 *   page component reads the hash client-side and posts the access token to
 *   the action below, which verifies it with GoTrue and sets the session.
 * - PKCE (?code=): exchanged entirely server-side when a verifier cookie is
 *   present, kept as a fallback for GoTrue deployments with PKCE enabled.
 */
export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const next = safeNext(url.searchParams.get("next"));

  const providerError =
    url.searchParams.get("error_description") || url.searchParams.get("error");
  if (providerError) {
    return { error: providerError.replace(/\+/g, " ") };
  }

  const code = url.searchParams.get("code");
  if (code) {
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

  // No ?code= and no provider error: assume implicit-flow fragment tokens,
  // which only the browser can see. Render the client-side handler.
  return { error: null };
}

/**
 * Implicit-flow completion: the page component posts the fragment access
 * token here. GoTrue authenticates it via /auth/v1/user; the token itself is
 * never stored — the session is our own rotated httpOnly cookie.
 */
export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const next = safeNext(url.searchParams.get("next"));

  const form = await request.formData();
  const accessToken = String(form.get("access_token") ?? "");
  if (!accessToken) {
    return { error: "No sign-in details were found. Please try again." };
  }

  const user = await verifyAccessToken(env, accessToken);
  if (!user) {
    return { error: "We couldn't verify your sign-in. Please try again." };
  }

  const session = await rotateSession(env, await getSession(request, env));
  session.set("user", JSON.stringify(user));
  throw redirect(next, {
    headers: { "Set-Cookie": await commitSession(env, session) },
  });
}

export default function AuthCallback({ loaderData, actionData }: Route.ComponentProps) {
  const submit = useSubmit();
  const [hashError, setHashError] = useState<string | null>(null);
  const error = loaderData.error ?? actionData?.error ?? hashError;

  useEffect(() => {
    if (loaderData.error) return;
    const raw = window.location.hash.startsWith("#") ? window.location.hash.slice(1) : "";
    const params = new URLSearchParams(raw);
    const desc = params.get("error_description") || params.get("error");
    if (desc) {
      setHashError(desc.replace(/\+/g, " "));
      return;
    }
    const token = params.get("access_token");
    if (!token) {
      setHashError("No sign-in details were found. Please try again.");
      return;
    }
    // Scrub the tokens from the address bar and history before the exchange.
    window.history.replaceState(null, "", window.location.pathname + window.location.search);
    const form = new FormData();
    form.set("access_token", token);
    submit(form, { method: "post" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (!error) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-background px-6">
        <div className="flex w-full max-w-sm flex-col items-center gap-5 rounded-2xl bg-card p-8 text-center shadow-04">
          <span className="flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-02">
            <span className="font-display text-2xl leading-none">D</span>
          </span>
          <div
            className="size-6 animate-spin rounded-full border-2 border-secondary border-t-primary"
            aria-hidden
          />
          <p className="body-sm text-muted-foreground">Signing you in…</p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-6">
      <div className="flex w-full max-w-sm flex-col items-center gap-5 rounded-2xl bg-card p-8 text-center shadow-04">
        <span className="flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-02">
          <span className="font-display text-2xl leading-none">D</span>
        </span>
        <div className="flex flex-col gap-2">
          <h1 className="text-h3 text-foreground">Sign-in failed</h1>
          <p className="body-sm text-muted-foreground">{error}</p>
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
