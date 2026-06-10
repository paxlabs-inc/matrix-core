// Augments the wrangler-generated `Env` with vars supplied via `.dev.vars`
// (local) or `wrangler secret` / dashboard (prod). These never live in
// wrangler.jsonc, so `wrangler types` does not know about them.
interface Env {
  SESSION_SECRET?: string;
  MARKETPLACE_DEV_AUTH?: string;
  SUPABASE_URL?: string;
  SUPABASE_ANON_KEY?: string;
}
