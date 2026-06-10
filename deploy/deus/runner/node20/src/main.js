import { dispatch } from './dispatch.js';

// Appwrite Node.js (node-20.0) function entrypoint.
//
// Appwrite invokes the default export with an execution context. The gateway
// (CallAppwriteExecution) sends the HostedInvokeRequest as the execution body
// with path "/invoke"; we parse it, dispatch to the developer handler, and
// return the HostedInvokeResponse envelope as JSON.
export default async ({ req, res, log, error }) => {
  let payload;
  try {
    payload = parsePayload(req);
  } catch (err) {
    if (typeof error === 'function') error(`invalid invoke payload: ${err}`);
    return res.json({ outcome: 'error', result: { error: 'invalid payload' }, units: '1' }, 400);
  }

  if (typeof log === 'function' && payload.invocation_id) {
    log(`invoke ${payload.operation || ''} (${payload.invocation_id})`);
  }

  const out = await dispatch(payload, process.env);
  const status = out.outcome === 'ok' ? 200 : 500;
  return res.json(out, status);
};

// parsePayload reads the invoke body across Appwrite runtime shapes: bodyJson
// (already-parsed object), bodyText / bodyRaw / body (string), or an object.
function parsePayload(req) {
  if (!req) return {};
  if (req.bodyJson && typeof req.bodyJson === 'object') return req.bodyJson;
  const raw = req.bodyText ?? req.bodyRaw ?? req.body ?? '';
  if (raw && typeof raw === 'object') return raw;
  if (typeof raw === 'string' && raw.trim().length) return JSON.parse(raw);
  return {};
}
