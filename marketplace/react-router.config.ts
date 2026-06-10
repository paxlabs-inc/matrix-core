import type { Config } from "@react-router/dev/config";

export default {
  // Config options...
  // Server-side render by default, to enable SPA mode set this to `false`
  ssr: true,
  future: {
    // Middleware mode swaps AppLoadContext for a RouterContextProvider, which
    // breaks the standard Cloudflare `{ cloudflare: { env, ctx } }` load-context
    // pattern. The edge BFF uses plain typed loaders/actions, so keep it off.
    v8_middleware: false,
    v8_passThroughRequests: true,
    v8_splitRouteModules: true,
    v8_trailingSlashAwareDataRequests: true,
    v8_viteEnvironmentApi: true,
  },
} satisfies Config;
