import type { AppLoadContext, EntryContext } from "react-router";
import { ServerRouter } from "react-router";
import { isbot } from "isbot";
import { renderToReadableStream } from "react-dom/server";
import { captureException } from "@/lib/sentry.server";

// Cloudflare Workers (workerd) is a Web-platform runtime: stream SSR with
// `renderToReadableStream`, not the Node-only `renderToPipeableStream`.
export default async function handleRequest(
  request: Request,
  responseStatusCode: number,
  responseHeaders: Headers,
  routerContext: EntryContext,
  loadContext: AppLoadContext
) {
  let shellRendered = false;
  const userAgent = request.headers.get("user-agent");
  // Matches the CSP header set in workers/app.ts; React stamps it onto every
  // inline hydration script and module preload it emits.
  const nonce = loadContext.cspNonce;

  const body = await renderToReadableStream(
    <ServerRouter context={routerContext} url={request.url} nonce={nonce} />,
    {
      nonce,
      onError(error: unknown) {
        responseStatusCode = 500;
        // Log streaming errors only once the shell has rendered.
        if (shellRendered) {
          console.error(error);
          loadContext.cloudflare.ctx.waitUntil(
            captureException(loadContext.cloudflare.env, error, {
              requestId: loadContext.requestId,
              url: request.url,
            })
          );
        }
      },
    }
  );
  shellRendered = true;

  // Bots and SPA-mode requests need the fully buffered document.
  if ((userAgent && isbot(userAgent)) || routerContext.isSpaMode) {
    await body.allReady;
  }

  responseHeaders.set("Content-Type", "text/html");
  return new Response(body, {
    headers: responseHeaders,
    status: responseStatusCode,
  });
}
