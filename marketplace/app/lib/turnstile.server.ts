/**
 * Cloudflare Turnstile server-side verification. Active only when the secret
 * is configured (wrangler secret put TURNSTILE_SECRET_KEY); otherwise the
 * check passes so local dev and pre-provisioning deploys keep working.
 */

export interface TurnstileEnv {
  TURNSTILE_SITE_KEY?: string;
  TURNSTILE_SECRET_KEY?: string;
}

const SITEVERIFY_URL = "https://challenges.cloudflare.com/turnstile/v0/siteverify";

export function turnstileEnabled(env: TurnstileEnv): boolean {
  return Boolean(env.TURNSTILE_SITE_KEY && env.TURNSTILE_SECRET_KEY);
}

export async function verifyTurnstile(
  env: TurnstileEnv,
  token: string | null,
  remoteIp?: string
): Promise<boolean> {
  if (!turnstileEnabled(env)) return true;
  if (!token) return false;
  try {
    const body = new URLSearchParams({
      secret: env.TURNSTILE_SECRET_KEY ?? "",
      response: token,
    });
    if (remoteIp) body.set("remoteip", remoteIp);
    const res = await fetch(SITEVERIFY_URL, { method: "POST", body });
    if (!res.ok) return false;
    const data = (await res.json()) as { success?: boolean };
    return data.success === true;
  } catch {
    // siteverify outage: fail open rather than locking everyone out.
    return true;
  }
}
