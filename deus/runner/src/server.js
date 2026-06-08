import http from 'node:http';
import { runHandle } from './harness.js';
import { handle } from './handler.js';

const port = Number(process.env.PORT || 8080);

const server = http.createServer(async (req, res) => {
  if (req.method !== 'POST' || req.url !== '/invoke') {
    res.writeHead(404);
    res.end('not found');
    return;
  }
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
});

server.listen(port, () => {
  console.log(`deus-runner listening on :${port}`);
});
