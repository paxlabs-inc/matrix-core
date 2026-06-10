import { createCookieSessionStorage, redirect } from "react-router";
import type { CallerIdentity, DeveloperIdentity } from "./deus.server";

export interface AppUser {
  id: string;
  email?: string;
  displayName?: string;
  did: string;
  provider?: string;
}

export interface AuthEnv {
  SESSION_SECRET?: string;
  SUPABASE_URL?: string;
  SUPABASE_ANON_KEY?: string;
  MARKETPLACE_DEV_AUTH?: string;
  DEUS_API_URL?: string;
}

const SESSION_COOKIE = "deus_session";

interface SessionData {
  user: string;
  wallet: string;
}
interface SessionFlash {
  error: string;
}

const typedFactory = createCookieSessionStorage<SessionData, SessionFlash>;
type AppSessionStorage = ReturnType<typeof typedFactory>;

let cached: { secret: string; storage: AppSessionStorage } | undefined;

function sessionStorage(env: AuthEnv): AppSessionStorage {
  const secret = env.SESSION_SECRET?.trim() || "deus-marketplace-dev-secret";
  if (cached && cached.secret === secret) return cached.storage;
  const storage = createCookieSessionStorage<SessionData, SessionFlash>({
    cookie: {
      name: SESSION_COOKIE,
      httpOnly: true,
      path: "/",
      sameSite: "lax",
      secrets: [secret],
      secure: !!env.SUPABASE_URL && process.env.NODE_ENV === "production",
      maxAge: 60 * 60 * 24 * 30,
    },
  });
  cached = { secret, storage };
  return storage;
}

export function isDevAuth(env: AuthEnv): boolean {
  // Dev auth is on unless explicitly disabled, OR whenever Supabase is unset.
  if (env.MARKETPLACE_DEV_AUTH === "0") return false;
  return env.MARKETPLACE_DEV_AUTH === "1" || !env.SUPABASE_URL;
}

export function getSession(request: Request, env: AuthEnv) {
  return sessionStorage(env).getSession(request.headers.get("Cookie"));
}

export function commitSession(env: AuthEnv, session: Awaited<ReturnType<typeof getSession>>) {
  return sessionStorage(env).commitSession(session);
}

export function destroySession(env: AuthEnv, session: Awaited<ReturnType<typeof getSession>>) {
  return sessionStorage(env).destroySession(session);
}

export async function getUser(request: Request, env: AuthEnv): Promise<AppUser | null> {
  const session = await getSession(request, env);
  const raw = session.get("user");
  if (!raw) return null;
  try {
    return typeof raw === "string" ? (JSON.parse(raw) as AppUser) : (raw as AppUser);
  } catch {
    return null;
  }
}

export async function getWallet(request: Request, env: AuthEnv): Promise<string | null> {
  const session = await getSession(request, env);
  const w = session.get("wallet");
  return typeof w === "string" && w.length > 0 ? w : null;
}

export async function requireUser(
  request: Request,
  env: AuthEnv
): Promise<AppUser> {
  const user = await getUser(request, env);
  if (!user) {
    const url = new URL(request.url);
    const next = encodeURIComponent(url.pathname + url.search);
    throw redirect(`/login?next=${next}`);
  }
  return user;
}

/** Build a stable Matrix-shaped DID for a user identity. */
export function deriveDid(seed: { wallet?: string; id?: string; email?: string }): string {
  if (seed.wallet) return `did:pkh:eip155:${seed.wallet.toLowerCase()}`;
  const base = (seed.id || seed.email || "anon").toLowerCase();
  return `did:matrix:marketplace:${hash8(base)}`;
}

/** Deus caller headers for the public try-it / invoke flow. */
export function callerIdentityFor(
  user: AppUser | null,
  wallet: string | null
): CallerIdentity {
  if (!user && !wallet) return {};
  const did = user?.did ?? deriveDid({ wallet: wallet ?? undefined });
  return {
    did,
    wallet: wallet ?? undefined,
  };
}

/** Deus developer headers for owner-scoped listing/hosting calls. */
export function developerIdentityFor(wallet: string | null): DeveloperIdentity {
  return { wallet: wallet ?? undefined };
}

// ─── Login flows ────────────────────────────────────────────────────────────

/** Dev login: trusted only when dev auth is enabled. */
export function devLoginUser(email: string): AppUser {
  const clean = email.trim().toLowerCase();
  return {
    id: hash8(clean),
    email: clean,
    displayName: clean.split("@")[0] || "developer",
    did: deriveDid({ email: clean }),
    provider: "dev",
  };
}

/** Supabase OAuth authorize URL (Google / GitHub). */
export function oauthAuthorizeUrl(
  env: AuthEnv,
  provider: "google" | "github",
  redirectTo: string
): string {
  const base = (env.SUPABASE_URL ?? "").replace(/\/+$/, "");
  const params = new URLSearchParams({ provider, redirect_to: redirectTo });
  return `${base}/auth/v1/authorize?${params.toString()}`;
}

/** Resolve a Supabase access token to an AppUser (production callback path). */
export async function userFromSupabaseToken(
  env: AuthEnv,
  accessToken: string
): Promise<AppUser | null> {
  const base = env.SUPABASE_URL?.replace(/\/+$/, "");
  if (!base || !env.SUPABASE_ANON_KEY) return null;
  try {
    const res = await fetch(`${base}/auth/v1/user`, {
      headers: {
        Authorization: `Bearer ${accessToken}`,
        apikey: env.SUPABASE_ANON_KEY,
      },
    });
    if (!res.ok) return null;
    const u = (await res.json()) as {
      id: string;
      email?: string;
      user_metadata?: { full_name?: string; name?: string };
      app_metadata?: { provider?: string };
    };
    return {
      id: u.id,
      email: u.email,
      displayName: u.user_metadata?.full_name || u.user_metadata?.name || u.email,
      did: deriveDid({ id: u.id, email: u.email }),
      provider: u.app_metadata?.provider,
    };
  } catch {
    return null;
  }
}

/** Tiny non-crypto hash for stable dev identifiers (not security-sensitive). */
function hash8(input: string): string {
  let h = 2166136261;
  for (let i = 0; i < input.length; i++) {
    h ^= input.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return (h >>> 0).toString(16).padStart(8, "0");
}
