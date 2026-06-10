import { useRouteLoaderData } from "react-router";
import type { loader as rootLoader } from "../root";

/**
 * Cloudflare Turnstile widget (implicit rendering). Renders nothing when no
 * site key is configured, so local dev and pre-provisioning deploys are
 * unaffected. Implicit mode injects a hidden `cf-turnstile-response` input
 * into the enclosing form; actions verify it via verifyTurnstile().
 */
export function TurnstileWidget({ siteKey }: { siteKey: string | null }) {
  const root = useRouteLoaderData<typeof rootLoader>("root");
  if (!siteKey) return null;
  return (
    <>
      <script
        src="https://challenges.cloudflare.com/turnstile/v0/api.js"
        async
        defer
        nonce={root?.cspNonce}
      />
      <div className="cf-turnstile" data-sitekey={siteKey} data-theme="dark" data-size="flexible" />
    </>
  );
}
