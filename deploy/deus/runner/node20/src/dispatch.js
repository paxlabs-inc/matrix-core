import crypto from 'node:crypto';
import { runHandle } from './harness.js';
import { handle } from './handler.js';

const DEFAULT_MAX_RESPONSE_BYTES = 262144;

// dispatch runs one invoke payload end-to-end and returns the
// HostedInvokeResponse envelope the gateway expects:
//   { outcome, result, units, runner_sig? }
// It is shared by both the Appwrite entrypoint (main.js) and the dev HTTP
// server (server.js) so they behave identically.
export async function dispatch(payload, env = process.env) {
  const operation = payload.operation;
  const args = payload.args || {};
  const ctx = {
    callerDid: payload.caller_did,
    invocationId: payload.invocation_id,
    deadlineMs: payload.deadline_ms || 5000,
    receiptDigest: payload.receipt_digest || '',
    logger: console,
    secrets: env,
  };

  let out;
  try {
    out = await runHandle(handle, operation, args, ctx);
  } catch (err) {
    return { outcome: 'error', result: { error: errMessage(err) }, units: '1' };
  }

  // Enforce the response cap (defense in depth; the gateway also caps the wire
  // size). Limit is provided as an Appwrite function variable on deploy.
  const maxBytes = positiveInt(env.DEUS_MAX_RESPONSE_BYTES, DEFAULT_MAX_RESPONSE_BYTES);
  const serialized = JSON.stringify(out.result ?? {});
  if (Buffer.byteLength(serialized, 'utf8') > maxBytes) {
    return { outcome: 'error', result: { error: 'response exceeds max bytes' }, units: out.units || '1' };
  }

  // Optional runner co-signature over the gateway-supplied receipt digest.
  // Stubbed as HMAC-SHA256; swap for the production runner key scheme later.
  const signingKey = env.RUNNER_SIGNING_KEY;
  if (signingKey && ctx.receiptDigest) {
    out.runner_sig = signReceipt(signingKey, ctx.receiptDigest);
  }
  return out;
}

function signReceipt(keyHex, digestHex) {
  const stripped = String(keyHex).replace(/^0x/, '');
  let key;
  try {
    key = Buffer.from(stripped, 'hex');
    if (key.length === 0) key = Buffer.from(String(keyHex), 'utf8');
  } catch {
    key = Buffer.from(String(keyHex), 'utf8');
  }
  const mac = crypto.createHmac('sha256', key);
  mac.update(String(digestHex));
  return '0x' + mac.digest('hex');
}

function errMessage(err) {
  if (err && err.message) return String(err.message);
  return String(err);
}

function positiveInt(value, fallback) {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}
