/**
 * Workers Rate Limiting bindings (wrangler.jsonc `unsafe.bindings`). Every
 * limiter is optional: local dev and tests run without bindings and the
 * guard no-ops, so the protection is purely additive in production.
 */

export interface RateLimiterBinding {
  limit(options: { key: string }): Promise<{ success: boolean }>;
}

export interface LimitsEnv {
  /** Login attempts (dev login + OAuth starts), keyed by client IP. */
  RL_LOGIN?: RateLimiterBinding;
  /** Wallet link/nonce attempts, keyed by client IP. */
  RL_WALLET?: RateLimiterBinding;
  /** Quote/invoke (spends money), keyed by wallet or IP. */
  RL_INVOKE?: RateLimiterBinding;
}

/** Best client identity for limiter keys behind Cloudflare. */
export function clientKey(request: Request): string {
  return (
    request.headers.get("CF-Connecting-IP") ??
    request.headers.get("X-Forwarded-For")?.split(",")[0]?.trim() ??
    "unknown"
  );
}

/**
 * Returns true when the request is allowed. Fails open on binding errors —
 * a rate-limiter outage must not take down login or invoke entirely (the
 * Cloudflare WAF layer above still applies).
 */
export async function allowRequest(
  binding: RateLimiterBinding | undefined,
  key: string
): Promise<boolean> {
  if (!binding) return true;
  try {
    const { success } = await binding.limit({ key });
    return success;
  } catch {
    return true;
  }
}
