import { createCookie, createCookieSessionStorage, redirect } from "react-router";
import { createWorkersKVSessionStorage } from "@react-router/cloudflare";
import type { CallerIdentity, DeveloperIdentity } from "./deus.server";

export interface AppUser {
  id: string;
  email?: string;
  displayName?: string;
  did: string;
  provider?: string;
}

export interface AuthEnv {
  ENVIRONMENT?: string;
  SESSION_SECRET?: string;
  SUPABASE_URL?: string;
  SUPABASE_ANON_KEY?: string;
  MARKETPLACE_DEV_AUTH?: string;
  DEUS_API_URL?: string;
  SESSIONS_KV?: KVNamespace;
}

const SESSION_COOKIE = "deus_session";
const SESSION_MAX_AGE_SECONDS = 60 * 60 * 24 * 7;

interface SessionData {
  user: string;
  wallet: string;
  /** Short-lived deusd developer auth token minted by SIWE verification. */
  developerToken: string;
}
interface SessionFlash {
  error: string;
}

interface SessionCookieOptions {
  name: string;
  httpOnly: boolean;
  path: string;
  sameSite: "lax";
  secrets: string[];
  secure: boolean;
  maxAge: number;
}

/**
 * KV-backed sessions when the SESSIONS_KV binding exists (prod: server-side
 * store, cookie carries only the session ID, revocation = KV delete). Pure
 * cookie sessions as the local-dev fallback.
 */
function createSessionStorageBackend(
  env: AuthEnv,
  cookie: SessionCookieOptions
): AppSessionStorage {
  if (env.SESSIONS_KV) {
    return createWorkersKVSessionStorage<SessionData, SessionFlash>({
      kv: env.SESSIONS_KV,
      cookie,
    });
  }
  return createCookieSessionStorage<SessionData, SessionFlash>({ cookie });
}

const typedFactory = createCookieSessionStorage<SessionData, SessionFlash>;
type AppSessionStorage = ReturnType<typeof typedFactory>;

/** Production is detected from the Worker var, never from process.env (which
 * may be undefined in workerd). Fail closed: unknown environment = prod. */
export function isProduction(env: AuthEnv): boolean {
  return env.ENVIRONMENT !== "development";
}

const MIN_SECRET_LENGTH = 16;

function resolveSessionSecret(env: AuthEnv): string {
  const secret = env.SESSION_SECRET?.trim() ?? "";
  if (secret.length >= MIN_SECRET_LENGTH) return secret;
  if (isProduction(env)) {
    throw new Error(
      "SESSION_SECRET is missing or too short in production. Set it via `wrangler secret put SESSION_SECRET`."
    );
  }
  return "deus-marketplace-dev-secret";
}

let cached: { key: string; storage: AppSessionStorage } | undefined;

function sessionStorage(env: AuthEnv): AppSessionStorage {
  const secret = resolveSessionSecret(env);
  const secure = isProduction(env);
  const key = `${secure}:${secret}`;
  if (cached && cached.key === key) return cached.storage;
  const storage = createSessionStorageBackend(env, {
    name: SESSION_COOKIE,
    httpOnly: true,
    path: "/",
    sameSite: "lax",
    secrets: [secret],
    secure,
    maxAge: SESSION_MAX_AGE_SECONDS,
  });
  cached = { key, storage };
  return storage;
}

export function isDevAuth(env: AuthEnv): boolean {
  // Fail closed: dev auth requires BOTH an explicit opt-in flag AND a
  // non-production environment. A misconfigured prod deploy can never
  // silently become "log in as anyone".
  return env.MARKETPLACE_DEV_AUTH === "1" && !isProduction(env);
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

/**
 * Login rotation: issue a brand-new session (fresh ID under KV storage) and
 * copy forward only the explicitly whitelisted keys. Defeats session fixation.
 */
export async function rotateSession(
  env: AuthEnv,
  old: Awaited<ReturnType<typeof getSession>>
): Promise<Awaited<ReturnType<typeof getSession>>> {
  const fresh = await sessionStorage(env).getSession();
  const wallet = old.get("wallet");
  const developerToken = old.get("developerToken");
  if (typeof wallet === "string" && wallet) fresh.set("wallet", wallet);
  if (typeof developerToken === "string" && developerToken) {
    fresh.set("developerToken", developerToken);
  }
  return fresh;
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

/** Developer identity (wallet + SIWE token) for owner-scoped deusd calls. */
export async function getDeveloperIdentity(
  request: Request,
  env: AuthEnv
): Promise<DeveloperIdentity> {
  const session = await getSession(request, env);
  const wallet = session.get("wallet");
  const token = session.get("developerToken");
  return {
    wallet: typeof wallet === "string" && wallet ? wallet : undefined,
    token: typeof token === "string" && token ? token : undefined,
  };
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

// ─── Supabase OAuth (PKCE authorization-code flow) ──────────────────────────
//
// The implicit flow put the access token in the URL fragment where client JS
// had to fish it out. PKCE keeps tokens off the URL entirely: we send a S256
// code challenge to GoTrue, it redirects back with ?code=, and the server
// exchanges code + verifier directly. The verifier lives in a short-lived
// signed httpOnly cookie (SameSite=lax survives the top-level redirect).

const PKCE_COOKIE = "sb_pkce";
const PKCE_MAX_AGE_SECONDS = 600;

function pkceCookie(env: AuthEnv) {
  return createCookie(PKCE_COOKIE, {
    httpOnly: true,
    path: "/",
    sameSite: "lax",
    secrets: [resolveSessionSecret(env)],
    secure: isProduction(env),
    maxAge: PKCE_MAX_AGE_SECONDS,
  });
}

function base64url(bytes: Uint8Array): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export interface PkcePair {
  verifier: string;
  challenge: string;
}

export async function generatePkce(): Promise<PkcePair> {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  const verifier = base64url(bytes);
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return { verifier, challenge: base64url(new Uint8Array(digest)) };
}

export function commitPkceVerifier(env: AuthEnv, verifier: string): Promise<string> {
  return pkceCookie(env).serialize(verifier);
}

export async function readPkceVerifier(env: AuthEnv, request: Request): Promise<string | null> {
  const v = await pkceCookie(env).parse(request.headers.get("Cookie"));
  return typeof v === "string" && v.length > 0 ? v : null;
}

export function clearPkceVerifier(env: AuthEnv): Promise<string> {
  return pkceCookie(env).serialize("", { maxAge: 0 });
}

/** Supabase OAuth authorize URL (Google / GitHub), PKCE S256. */
export function oauthAuthorizeUrl(
  env: AuthEnv,
  provider: "google" | "github",
  redirectTo: string,
  codeChallenge: string
): string {
  const base = (env.SUPABASE_URL ?? "").replace(/\/+$/, "");
  const params = new URLSearchParams({
    provider,
    redirect_to: redirectTo,
    code_challenge: codeChallenge,
    code_challenge_method: "s256",
  });
  return `${base}/auth/v1/authorize?${params.toString()}`;
}

interface SupabaseUserPayload {
  id: string;
  email?: string;
  user_metadata?: { full_name?: string; name?: string };
  app_metadata?: { provider?: string };
}

function appUserFromSupabase(u: SupabaseUserPayload): AppUser {
  return {
    id: u.id,
    email: u.email,
    displayName: u.user_metadata?.full_name || u.user_metadata?.name || u.email,
    did: deriveDid({ id: u.id, email: u.email }),
    provider: u.app_metadata?.provider,
  };
}

/**
 * Exchange the PKCE authorization code for tokens server-side. The token never
 * touches a URL or client JS; GoTrue validates code + verifier + expiry.
 */
export async function exchangeOAuthCode(
  env: AuthEnv,
  code: string,
  verifier: string
): Promise<AppUser | null> {
  const base = env.SUPABASE_URL?.replace(/\/+$/, "");
  if (!base || !env.SUPABASE_ANON_KEY) return null;
  try {
    const res = await fetch(`${base}/auth/v1/token?grant_type=pkce`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        apikey: env.SUPABASE_ANON_KEY,
      },
      body: JSON.stringify({ auth_code: code, code_verifier: verifier }),
    });
    if (!res.ok) return null;
    const data = (await res.json()) as { user?: SupabaseUserPayload };
    if (!data.user?.id) return null;
    return appUserFromSupabase(data.user);
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
