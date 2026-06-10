import type { AppLoadContext } from "react-router";

/**
 * Resolve the Worker environment (bindings + vars) from the RR load context.
 * Bindings are injected via `loadContext` in `workers/app.ts`, never imported
 * globally — keeps the server build portable and testable.
 */
export function getEnv(context: AppLoadContext): Env {
  return context.cloudflare.env;
}
