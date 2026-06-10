/**
 * One-shot form tokens: double-submit / browser-resubmit protection for
 * money-adjacent actions (payout, create listing). Tokens are minted in the
 * loader and claimed exactly once in the action. Claims live in CACHE_KV when
 * bound; without KV (local dev) claiming always succeeds — the disabled
 * submit button remains the only guard there, which is fine for dev.
 */

import type { CacheEnv } from "./cache.server";

const CLAIM_TTL_SECONDS = 3600;
const TOKEN_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export function mintFormToken(): string {
  return crypto.randomUUID();
}

/** True when the token is well-formed and has not been used before. */
export async function claimFormToken(env: CacheEnv, token: string): Promise<boolean> {
  if (!TOKEN_RE.test(token)) return false;
  if (!env.CACHE_KV) return true;
  const key = `form-token:${token}`;
  try {
    const existing = await env.CACHE_KV.get(key);
    if (existing !== null) return false;
    await env.CACHE_KV.put(key, "1", { expirationTtl: CLAIM_TTL_SECONDS });
    return true;
  } catch {
    return true; // KV outage: fail open, button-disable still guards
  }
}
