// Bindings that are provisioned post-launch and therefore commented out in
// wrangler.jsonc (see OPS_RUNBOOK.md). Declared optional here via interface
// merging with the generated Env so code compiles and no-ops until the
// namespaces exist; `wrangler types` owns worker-configuration.d.ts.
interface Env {
  SESSIONS_KV?: KVNamespace;
  CACHE_KV?: KVNamespace;
  /** Secrets set via `wrangler secret put` (invisible to typegen). */
  SENTRY_DSN?: string;
  TURNSTILE_SECRET_KEY?: string;
}
