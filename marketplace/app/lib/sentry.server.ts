/**
 * Minimal, dependency-free Sentry error reporting. Active only when
 * SENTRY_DSN is set (wrangler secret); otherwise a no-op. Uses the plain
 * store API — enough for "errors show up with a stack trace" until a full
 * @sentry/cloudflare integration is warranted.
 */

export interface SentryEnv {
  SENTRY_DSN?: string;
  ENVIRONMENT?: string;
}

interface ParsedDsn {
  publicKey: string;
  host: string;
  projectId: string;
  protocol: string;
}

function parseDsn(dsn: string): ParsedDsn | null {
  try {
    const url = new URL(dsn);
    const projectId = url.pathname.replace(/^\//, "");
    if (!url.username || !projectId) return null;
    return {
      publicKey: url.username,
      host: url.host,
      projectId,
      protocol: url.protocol.replace(":", ""),
    };
  } catch {
    return null;
  }
}

/** Fire-and-forget capture; callers wrap in ctx.waitUntil(). */
export async function captureException(
  env: SentryEnv,
  error: unknown,
  context: { requestId?: string; url?: string } = {}
): Promise<void> {
  const dsn = env.SENTRY_DSN ? parseDsn(env.SENTRY_DSN) : null;
  if (!dsn) return;

  const err = error instanceof Error ? error : new Error(String(error));
  const event = {
    event_id: crypto.randomUUID().replaceAll("-", ""),
    timestamp: new Date().toISOString(),
    platform: "javascript",
    environment: env.ENVIRONMENT ?? "unknown",
    tags: { request_id: context.requestId },
    request: context.url ? { url: context.url } : undefined,
    exception: {
      values: [
        {
          type: err.name,
          value: err.message,
          stacktrace: err.stack
            ? { frames: [{ filename: "stack", function: err.stack.slice(0, 4096) }] }
            : undefined,
        },
      ],
    },
  };

  try {
    await fetch(`${dsn.protocol}://${dsn.host}/api/${dsn.projectId}/store/`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Sentry-Auth": `Sentry sentry_version=7, sentry_key=${dsn.publicKey}, sentry_client=deus-marketplace/1.0`,
      },
      body: JSON.stringify(event),
    });
  } catch {
    // Reporting must never take the app down.
  }
}
