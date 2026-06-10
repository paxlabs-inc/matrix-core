import { useEffect, useRef, useState } from "react";
import { Link, redirect, useFetcher, useNavigate } from "react-router";
import type { Route } from "./+types/auth.callback";
import { getEnv } from "@/lib/env";
import { commitSession, getSession, userFromSupabaseToken } from "@/lib/auth.server";
import { Spinner } from "@/components/ui";

function safeNext(value: string | null | undefined): string {
  if (value && value.startsWith("/") && !value.startsWith("//")) return value;
  return "/dashboard";
}

export function loader({ request }: Route.LoaderArgs) {
  const url = new URL(request.url);
  return { next: safeNext(url.searchParams.get("next")) };
}

export async function action({ request, context }: Route.ActionArgs) {
  const env = getEnv(context);
  const url = new URL(request.url);
  const form = await request.formData();
  const token = String(form.get("access_token") ?? "");
  const next = safeNext((form.get("next") as string) || url.searchParams.get("next"));

  if (!token) return { error: "Missing sign-in token." };
  const user = await userFromSupabaseToken(env, token);
  if (!user) return { error: "We couldn't verify your sign-in. Please try again." };

  const session = await getSession(request, env);
  session.set("user", JSON.stringify(user));
  return redirect(next, {
    headers: { "Set-Cookie": await commitSession(env, session) },
  });
}

export default function AuthCallback({ loaderData }: Route.ComponentProps) {
  const fetcher = useFetcher<typeof action>();
  const navigate = useNavigate();
  const submitted = useRef(false);
  const [noToken, setNoToken] = useState(false);

  useEffect(() => {
    if (submitted.current) return;
    submitted.current = true;

    const hash = window.location.hash.startsWith("#")
      ? window.location.hash.slice(1)
      : window.location.hash;
    const params = new URLSearchParams(hash);
    const token = params.get("access_token");

    if (token) {
      fetcher.submit(
        { access_token: token, next: loaderData.next },
        { method: "post" }
      );
    } else {
      setNoToken(true);
      const t = setTimeout(() => navigate("/login", { replace: true }), 1200);
      return () => clearTimeout(t);
    }
  }, [fetcher, loaderData.next, navigate]);

  const error = fetcher.data?.error;

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-6">
      <div className="flex w-full max-w-sm flex-col items-center gap-5 rounded-2xl bg-card p-8 text-center shadow-04">
        <span className="flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-02">
          <span className="font-display text-2xl leading-none">D</span>
        </span>
        {error || noToken ? (
          <>
            <div className="flex flex-col gap-2">
              <h1 className="text-h3 text-foreground">Sign-in failed</h1>
              <p className="body-sm text-muted-foreground">
                {error ?? "No sign-in details were found."}
              </p>
            </div>
            <Link
              to="/login"
              className="text-sm font-medium text-primary underline-offset-4 hover:underline"
            >
              Back to sign in
            </Link>
          </>
        ) : (
          <>
            <Spinner className="size-6 text-primary" />
            <div className="flex flex-col gap-2">
              <h1 className="text-h3 text-foreground">Completing sign-in…</h1>
              <p className="body-sm text-muted-foreground">Hang tight, this only takes a moment.</p>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
