import http from 'node:http';
import { dispatch } from './dispatch.js';

// Local dev HTTP server. Exposes the same POST /invoke contract the gateway's
// dev path (CallHosted -> {url}/invoke) expects, sharing dispatch() with the
// Appwrite entrypoint so behavior matches production. Not used in Appwrite,
// which imports src/main.js directly.
const port = Number(process.env.PORT || 3000);

http
  .createServer(async (req, res) => {
    if (req.method !== 'POST' || req.url !== '/invoke') {
      res.writeHead(404).end('not found');
      return;
    }
    let payload;
    try {
      payload = JSON.parse((await readBody(req)) || '{}');
    } catch {
      res.writeHead(400, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ outcome: 'error', result: { error: 'invalid payload' }, units: '1' }));
      return;
    }
    const out = await dispatch(payload, process.env);
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(out));
  })
  .listen(port, () => {
    console.log(`deus-runner listening on :${port}`);
  });

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}
