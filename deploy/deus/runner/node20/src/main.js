import { runHandle } from './harness.js';
import { handle } from './handler.js';
import http from 'node:http';

const port = Number(process.env.PORT || 3000);

http.createServer(async (req, res) => {
  if (req.method !== 'POST' || req.url !== '/invoke') {
    res.writeHead(404).end('not found');
    return;
  }
  const raw = await readBody(req);
  const body = JSON.parse(raw);
  const out = await runHandle(handle, body.operation, body.args || {}, {
    callerDid: body.caller_did,
    invocationId: body.invocation_id,
    deadlineMs: body.deadline_ms || 5000,
    logger: console,
    secrets: process.env,
  });
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(out));
}).listen(port);

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}
