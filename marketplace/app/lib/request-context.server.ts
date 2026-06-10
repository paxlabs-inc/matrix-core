import { AsyncLocalStorage } from "node:async_hooks";

/**
 * Per-request context (workerd supports AsyncLocalStorage under
 * nodejs_compat). The worker entry seeds the request ID; the Deus client
 * propagates it to the backend as X-Request-ID so one ID follows a request
 * from edge log line to deusd log line.
 */

export interface RequestContext {
  requestId: string;
}

export const requestContext = new AsyncLocalStorage<RequestContext>();

export function currentRequestId(): string | undefined {
  return requestContext.getStore()?.requestId;
}
