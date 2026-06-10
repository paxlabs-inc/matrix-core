import {
  isRouteErrorResponse,
  Links,
  Meta,
  Outlet,
  Scripts,
  ScrollRestoration,
  useRouteLoaderData,
} from "react-router";

import { MotionConfig } from "motion/react";

import type { Route } from "./+types/root";
import "./fonts.css";
import "./app.css";
import "./smoothui.css";
import "./motion.css";

export function loader({ context }: Route.LoaderArgs) {
  return { cspNonce: context.cspNonce };
}

export const links: Route.LinksFunction = () => [
  // Preload the two fonts on the critical path (body + display) to avoid
  // FOIT; everything is same-origin now (no Google Fonts request).
  {
    rel: "preload",
    href: "/fonts/Inter-Variable-latin.woff2",
    as: "font",
    type: "font/woff2",
    crossOrigin: "anonymous",
  },
  {
    rel: "preload",
    href: "/fonts/PPPangramSansRounded-Semibold.otf",
    as: "font",
    type: "font/otf",
    crossOrigin: "anonymous",
  },
  { rel: "icon", href: "/favicon.ico", sizes: "48x48" },
  { rel: "icon", href: "/icon.svg", type: "image/svg+xml" },
  { rel: "apple-touch-icon", href: "/icon.svg" },
  { rel: "manifest", href: "/site.webmanifest" },
];

export function Layout({ children }: { children: React.ReactNode }) {
  // Available on normal renders; undefined on root-level error boundaries,
  // where a blocked hydration script is an acceptable degradation.
  const data = useRouteLoaderData<typeof loader>("root");
  const nonce = data?.cspNonce;
  return (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <meta name="theme-color" content="#0A0C10" />
        <Meta />
        <Links />
      </head>
      <body>
        {children}
        <ScrollRestoration nonce={nonce} />
        <Scripts nonce={nonce} />
      </body>
    </html>
  );
}

export default function App() {
  // reducedMotion="user" makes every motion/react animation respect the OS
  // prefers-reduced-motion setting; motion.css covers plain CSS animation.
  return (
    <MotionConfig reducedMotion="user">
      <Outlet />
    </MotionConfig>
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  let status = 500;
  let title = "Something went wrong";
  let details = "An unexpected error occurred on our side. Please try again in a moment.";
  let stack: string | undefined;

  if (isRouteErrorResponse(error)) {
    status = error.status;
    if (status === 404) {
      title = "Page not found";
      details = "That page doesn't exist — it may have been delisted or the link is stale.";
    } else if (status === 503) {
      title = "Temporarily unavailable";
      details = "The marketplace backend is catching its breath. Refresh in a few seconds.";
    } else {
      title = `Error ${status}`;
      details = error.statusText || details;
    }
  } else if (import.meta.env.DEV && error && error instanceof Error) {
    details = error.message;
    stack = error.stack;
  }

  return (
    <main className="flex min-h-screen items-center justify-center bg-background px-6">
      <div className="flex w-full max-w-md flex-col items-center gap-6 rounded-2xl bg-card p-10 text-center shadow-04">
        <span className="flex size-12 items-center justify-center rounded-2xl bg-primary text-primary-foreground shadow-02">
          <span className="font-display text-2xl leading-none">D</span>
        </span>
        <div className="flex flex-col gap-2">
          <p className="eyebrow">{status}</p>
          <h1 className="text-h2 text-foreground">{title}</h1>
          <p className="body-sm text-muted-foreground">{details}</p>
        </div>
        <div className="flex items-center gap-4">
          <a
            href="/"
            className="text-sm font-medium text-primary underline-offset-4 hover:underline"
          >
            Back to marketplace
          </a>
          <a
            href="/catalog"
            className="text-sm font-medium text-muted-foreground underline-offset-4 hover:underline"
          >
            Browse the catalog
          </a>
        </div>
        {stack && (
          <pre className="mono max-h-72 w-full overflow-auto rounded-lg bg-secondary p-4 text-left text-xs text-muted-foreground">
            <code>{stack}</code>
          </pre>
        )}
      </div>
    </main>
  );
}
