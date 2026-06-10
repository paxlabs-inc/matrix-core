/**
 * Short-TTL server-side data cache for hot Deus reads (catalog, featured).
 * Backed by the CACHE_KV namespace when bound; falls back to a per-isolate
 * in-memory map locally. Stale entries are served when the backend errors,
 * shielding public pages from origin blips.
 */

export interface CacheEnv {
  CACHE_KV?: KVNamespace;
}

interface Envelope<T> {
  storedAt: number;
  data: T;
}

const memoryCache = new Map<string, Envelope<unknown>>();
/** KV retention horizon; freshness is governed by the caller's TTL. */
const KV_EXPIRATION_SECONDS = 86_400;

async function readEnvelope<T>(env: CacheEnv, key: string): Promise<Envelope<T> | null> {
  if (env.CACHE_KV) {
    try {
      return await env.CACHE_KV.get<Envelope<T>>(key, "json");
    } catch {
      return null;
    }
  }
  return (memoryCache.get(key) as Envelope<T> | undefined) ?? null;
}

async function writeEnvelope<T>(env: CacheEnv, key: string, data: T): Promise<void> {
  const envelope: Envelope<T> = { storedAt: Date.now(), data };
  if (env.CACHE_KV) {
    try {
      await env.CACHE_KV.put(key, JSON.stringify(envelope), {
        expirationTtl: KV_EXPIRATION_SECONDS,
      });
    } catch {
      // cache write failures are non-fatal
    }
    return;
  }
  memoryCache.set(key, envelope);
}

/**
 * Read-through cache: fresh hits return immediately; misses populate the
 * cache; fetcher failures fall back to any stale copy before rethrowing.
 */
export async function cachedJson<T>(
  env: CacheEnv,
  key: string,
  ttlSeconds: number,
  fetcher: () => Promise<T>
): Promise<T> {
  const cached = await readEnvelope<T>(env, key);
  const ageSeconds = cached ? (Date.now() - cached.storedAt) / 1000 : Number.POSITIVE_INFINITY;

  if (cached && ageSeconds < ttlSeconds) {
    return cached.data;
  }

  try {
    const fresh = await fetcher();
    await writeEnvelope(env, key, fresh);
    return fresh;
  } catch (err) {
    if (cached) return cached.data; // serve stale on backend error
    throw err;
  }
}
