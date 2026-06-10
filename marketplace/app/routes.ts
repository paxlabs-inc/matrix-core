import {
  type RouteConfig,
  index,
  layout,
  prefix,
  route,
} from "@react-router/dev/routes";

export default [
  // ─── Public marketplace (shared chrome) ───────────────────────────────
  layout("routes/_public.tsx", [
    index("routes/home.tsx"),
    route("discover", "routes/discover.tsx"),
    route("catalog", "routes/catalog.tsx"),
    route("services/:id", "routes/service.tsx"),
  ]),

  // ─── Auth ──────────────────────────────────────────────────────────────
  route("login", "routes/login.tsx"),
  route("logout", "routes/logout.tsx"),
  route("auth/callback", "routes/auth.callback.tsx"),

  // ─── Resource routes (no UI) ────────────────────────────────────────────
  route("api/wallet", "routes/api.wallet.tsx"),
  route("robots.txt", "routes/robots.tsx"),
  route("sitemap.xml", "routes/sitemap.tsx"),
  route("healthz", "routes/healthz.tsx"),

  // ─── Authenticated dev dashboard ────────────────────────────────────────
  ...prefix("dashboard", [
    layout("routes/dashboard/_layout.tsx", [
      index("routes/dashboard/index.tsx"),
      route("services/new", "routes/dashboard/new.tsx"),
      route("services/:id", "routes/dashboard/service.tsx"),
      route("services/:id/analytics", "routes/dashboard/analytics.tsx"),
      route("earnings", "routes/dashboard/earnings.tsx"),
      route("account", "routes/dashboard/account.tsx"),
    ]),
  ]),
] satisfies RouteConfig;
