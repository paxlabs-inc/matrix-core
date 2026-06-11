import { useState } from "react";
import { data, Form, Link, redirect } from "react-router";
import SmoothButton from "@repo/smoothui/components/smooth-button";
import AnimatedInput from "../../components/ui/smoothui/animated-input";
import { ActionToast } from "@/components/feedback";
import type { Route } from "./+types/login";
import { getEnv } from "@/lib/env";
import {
  commitSession,
  devLoginUser,
  getSession,
  getUser,
  isDevAuth,
  oauthAuthorizeUrl,
  rotateSession,
} from "@/lib/auth.server";
import { allowRequest, clientKey } from "@/lib/limits.server";
import { emailSchema } from "@/lib/validate.server";
import { verifyTurnstile } from "@/lib/turnstile.server";
import { TurnstileWidget } from "@/components/turnstile";

/** Only allow same-site relative redirects to avoid open-redirect abuse. */
function safeNext(value: string | null | undefined): string {
  if (value && value.startsWith("/") && !value.startsWith("//")) return value;
  return "/dashboard";
}

export function meta() {
  return [{ title: "Sign in · Deus" }];
}

/** Auth surface — never cacheable. */
export function headers() {
  return { "Cache-Control": "private, no-store" };
}

export async function loader({ request, context }: Route.LoaderArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const next = safeNext(url.searchParams.get("next"));

  const user = await getUser(request, env);
  if (user) throw redirect(next);

  // Implicit flow: GoTrue returns tokens in the URL fragment to the callback
  // page, which posts them to the server for verification.
  const callback = `${url.origin}/auth/callback?next=${encodeURIComponent(next)}`;
  return data({
    isDev: isDevAuth(env),
    next,
    turnstileSiteKey: env.TURNSTILE_SITE_KEY || null,
    oauth: {
      google: oauthAuthorizeUrl(env, "google", callback),
      github: oauthAuthorizeUrl(env, "github", callback),
    },
  });
}

export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);

  // The dev email login is an explicit local-only backdoor. Without this
  // gate a direct POST /login would sign in as anyone, even in production.
  if (!isDevAuth(env)) {
    throw new Response("Not Found", { status: 404 });
  }

  const ip = clientKey(request);
  if (!(await allowRequest(env.RL_LOGIN, ip))) {
    return { error: "Too many sign-in attempts. Try again in a minute." };
  }

  const url = new URL(request.url);
  const form = await request.formData();

  const captcha = String(form.get("cf-turnstile-response") ?? "") || null;
  if (!(await verifyTurnstile(env, captcha, ip))) {
    return { error: "Verification failed. Please complete the challenge and retry." };
  }

  const next = safeNext((form.get("next") as string) || url.searchParams.get("next"));
  const parsedEmail = emailSchema.safeParse(String(form.get("email") ?? ""));
  if (!parsedEmail.success) {
    return { error: "Enter a valid email address to continue." };
  }

  const user = devLoginUser(parsedEmail.data);
  const session = await rotateSession(env, await getSession(request, env));
  session.set("user", JSON.stringify(user));
  return redirect(next, {
    headers: { "Set-Cookie": await commitSession(env, session) },
  });
}

function GoogleMark() {
  return (
    <svg viewBox="0 0 24 24" className="size-4" aria-hidden>
      <path
        fill="#EA4335"
        d="M12 10.2v3.9h5.5c-.24 1.4-1.7 4.1-5.5 4.1-3.3 0-6-2.7-6-6.1s2.7-6.1 6-6.1c1.9 0 3.1.8 3.8 1.5l2.6-2.5C17 1.9 14.7 1 12 1 6.9 1 2.8 5.1 2.8 12S6.9 23 12 23c6.4 0 8.9-4.5 8.9-7.7 0-.5-.05-.9-.12-1.3H12z"
      />
    </svg>
  );
}

function GithubMark() {
  return (
    <svg viewBox="0 0 24 24" className="size-4" fill="currentColor" aria-hidden>
      <path d="M12 1.5a10.5 10.5 0 0 0-3.32 20.46c.53.1.72-.23.72-.5l-.01-1.95c-2.92.64-3.54-1.25-3.54-1.25-.48-1.21-1.17-1.54-1.17-1.54-.95-.65.08-.64.08-.64 1.05.07 1.6 1.08 1.6 1.08.94 1.6 2.46 1.14 3.06.87.1-.68.37-1.14.66-1.4-2.33-.27-4.78-1.17-4.78-5.18 0-1.15.41-2.08 1.08-2.82-.11-.27-.47-1.34.1-2.79 0 0 .88-.28 2.88 1.07a9.9 9.9 0 0 1 5.24 0c2-1.35 2.88-1.07 2.88-1.07.57 1.45.21 2.52.1 2.79.67.74 1.07 1.67 1.07 2.82 0 4.02-2.45 4.9-4.79 5.16.38.33.71.97.71 1.96l-.01 2.9c0 .28.19.61.73.5A10.5 10.5 0 0 0 12 1.5Z" />
    </svg>
  );
}

export default function Login({ loaderData, actionData }: Route.ComponentProps) {
  const { isDev, oauth, next, turnstileSiteKey } = loaderData;
  const [email, setEmail] = useState("");

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-6 py-12">
      <div className="flex w-full max-w-md flex-col gap-8">
        <div className="flex flex-col items-center gap-4 text-center">
          <span className="flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-03">
            <span className="font-display text-2xl leading-none">D</span>
          </span>
          <div className="flex flex-col gap-2">
            <h1 className="text-h2 text-foreground">Sign in to Deus</h1>
            <p className="body-sm text-muted-foreground">
              Manage your listings, deployments, and earnings.
            </p>
          </div>
        </div>

        <div className="flex flex-col gap-5 rounded-2xl bg-card p-6 shadow-04 sm:p-8">
          <div className="flex flex-col gap-3">
            <SmoothButton asChild variant="outline" size="lg" className="w-full justify-center">
              <a href={oauth.google}>
                <GoogleMark />
                Continue with Google
              </a>
            </SmoothButton>
            <SmoothButton asChild variant="outline" size="lg" className="w-full justify-center">
              <a href={oauth.github}>
                <GithubMark />
                Continue with GitHub
              </a>
            </SmoothButton>
          </div>

          {isDev ? (
            <>
              <div className="flex items-center gap-3">
                <span className="h-px flex-1 bg-secondary" />
                <span className="eyebrow">or dev sign-in</span>
                <span className="h-px flex-1 bg-secondary" />
              </div>

              <Form method="post" className="flex flex-col gap-4 pt-2">
                <input type="hidden" name="next" value={next} />
                <input type="hidden" name="email" value={email} />
                <AnimatedInput
                  label="Email"
                  value={email}
                  onChange={setEmail}
                  placeholder="you@studio.dev"
                />
                <TurnstileWidget siteKey={turnstileSiteKey} />
                <ActionToast message={actionData?.error} type="error" />
                <SmoothButton type="submit" size="lg" className="w-full justify-center">
                  Continue
                </SmoothButton>
              </Form>
            </>
          ) : null}
        </div>

        <p className="text-center text-xs text-muted-foreground">
          <Link to="/" className="transition-colors hover:text-foreground">
            ← Back to marketplace
          </Link>
        </p>
      </div>
    </div>
  );
}
