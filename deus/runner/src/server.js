import http from 'node:http';
import { runHandle } from './harness.js';
import { handle } from './handler.js';
import { createLXP } from './lxp.js';

const port = Number(process.env.PORT || 8080);

async function handleInvoke(req, res) {
  const chunks = [];
  for await (const chunk of req) {
    chunks.push(chunk);
  }
  let body;
  try {
    body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    res.writeHead(400);
    res.end('invalid json');
    return;
  }
  try {
    const out = await runHandle(handle, body.operation, body.args || {}, {
      callerDid: body.caller_did,
      invocationId: body.invocation_id,
      deadlineMs: body.deadline_ms || 5000,
      logger: console,
      secrets: {},
    });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(out));
  } catch (err) {
    res.writeHead(503, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ outcome: 'error', message: String(err.message || err) }));
  }
}

// Self-hosted paywall (lxp/1): set LXP_LAYERX_URL + LXP_PRICE_USDX +
// LXP_PAY_TO to charge for /invoke without the deus gateway in the data path.
// Gateway-hosted runners leave this off — the gateway settles for them.
let invoke = handleInvoke;
if (process.env.LXP_LAYERX_URL && process.env.LXP_PRICE_USDX && process.env.LXP_PAY_TO) {
  const lxp = createLXP({
    layerxUrl: process.env.LXP_LAYERX_URL,
    bearer: process.env.LXP_LAYERX_BEARER,
    keyHex: process.env.LXP_KEY,
    didLabel: process.env.LXP_DID_LABEL || 'deus-runner',
  });
  invoke = lxp.guard(
    () => ({
      amount_usdx: process.env.LXP_PRICE_USDX,
      pay_to: process.env.LXP_PAY_TO,
      mode: process.env.LXP_MODE || 'exact',
      ttl_s: Number(process.env.LXP_HOLD_TTL_S || 0),
    }),
    handleInvoke,
  );
}

const server = http.createServer(async (req, res) => {
  if (req.method !== 'POST' || req.url !== '/invoke') {
    res.writeHead(404);
    res.end('not found');
    return;
  }
  try {
    await invoke(req, res);
  } catch (err) {
    res.writeHead(503, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ outcome: 'error', message: String(err.message || err) }));
  }
});

server.listen(port, () => {
  console.log(`deus-runner listening on :${port}`);
});
