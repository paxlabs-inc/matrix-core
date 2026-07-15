import test from 'node:test'
import assert from 'node:assert/strict'
import { once } from 'node:events'
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import http from 'node:http'
import { join } from 'node:path'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import { SandboxManager } from '../src/manager.mjs'
import { createServer } from '../src/server.mjs'

function request(socketPath, path, options = {}) {
  return new Promise((resolve, reject) => {
    const req = http.request({ socketPath, path, method: options.method || 'GET', headers: options.headers }, (res) => {
      const chunks = []
      res.on('data', (chunk) => chunks.push(chunk))
      res.on('end', () => {
        const raw = Buffer.concat(chunks).toString('utf8')
        resolve({ status: res.statusCode, json: () => JSON.parse(raw) })
      })
    })
    req.on('error', reject)
    if (options.body) req.write(options.body)
    req.end()
  })
}

test('real server and manager enforce auth and user-scoped list', async (t) => {
  const config = {
    token: 'transport-secret', previewDomain: 'preview.matrix.test', publicScheme: 'https',
    maxProxyBodyBytes: 1024, maxPerUser: 2, maxFiles: 10, maxUploadBytes: 1024,
    defaultTTLSeconds: 600, maxTTLSeconds: 3600, railwayToken: '', railwayAuthType: 'bearer', environmentId: 'env-test',
  }
  const manager = new SandboxManager(config)
  manager.records.set('a'.repeat(32), {
    slug: 'a'.repeat(32), userId: 'alice', status: 'ready', port: 3000,
    createdAt: '2026-07-15T00:00:00.000Z', expiresAt: '2026-07-15T01:00:00.000Z',
  })
  const server = createServer(config, manager)
  const socketDir = mkdtempSync('/tmp/matrix-sandboxd-test-')
  const socketPath = join(socketDir, 'server.sock')
  server.listen(socketPath)
  try {
    await once(server, 'listening')
  } catch (error) {
    rmSync(socketDir, { recursive: true, force: true })
    if (error?.code === 'EPERM') return t.skip('execution sandbox does not permit listening sockets')
    throw error
  }
  t.after(() => {
    server.close()
    rmSync(socketDir, { recursive: true, force: true })
  })

  const health = await request(socketPath, '/healthz')
  assert.equal(health.status, 200)

  const denied = await request(socketPath, '/v1/sandboxes')
  assert.equal(denied.status, 401)

  const listed = await request(socketPath, '/v1/sandboxes', {
    headers: { Authorization: 'Bearer transport-secret', 'X-Matrix-User': 'alice' },
  })
  assert.equal(listed.status, 200)
  const body = listed.json()
  assert.equal(body.sandboxes.length, 1)
  assert.equal(body.sandboxes[0].preview_url, `https://${'a'.repeat(32)}.preview.matrix.test/`)

  const other = await request(socketPath, '/v1/sandboxes', {
    headers: { Authorization: 'Bearer transport-secret', 'X-Matrix-User': 'bob' },
  })
  assert.deepEqual(other.json(), { sandboxes: [] })

  const invalid = await request(socketPath, '/v1/sandboxes', {
    method: 'POST',
    headers: { Authorization: 'Bearer transport-secret', 'X-Matrix-User': 'alice', 'Content-Type': 'application/json' },
    body: '{}',
  })
  assert.equal(invalid.status, 400)
  assert.deepEqual(invalid.json(), {
    ok: false,
    status: 'error',
    error: { code: 'INVALID_REQUEST', stage: 'request', message: 'port must be an integer from 1 to 65535' },
  })

  const bridge = spawn(process.execPath, [fileURLToPath(new URL('../../tools/sandbox/sandbox.mjs', import.meta.url))], {
    env: {
      ...process.env,
      MATRIX_SANDBOX_URL: 'http://sandboxd.test',
      MATRIX_SANDBOX_TOKEN: 'transport-secret',
      MATRIX_USER_ID: 'alice',
      MATRIX_WORKSPACE_ROOT: '/root/matrix',
    },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  t.after(() => bridge.kill())
  const lines = createInterface({ input: bridge.stdout })[Symbol.asyncIterator]()
  bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list', params: {} }) + '\n')
  const advertised = JSON.parse((await lines.next()).value)
  assert.deepEqual(advertised.result.tools.map((tool) => tool.name), ['preview_app'])
  bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'preview_app', arguments: { app_directory: '/outside' } } }) + '\n')
  const called = JSON.parse((await lines.next()).value)
  const envelope = JSON.parse(called.result.content[0].text)
  assert.equal(called.result.isError, true)
  assert.deepEqual(envelope, {
    ok: false,
    status: 'error',
    error: {
      code: 'APP_OUTSIDE_WORKSPACE',
      stage: 'inspect',
      message: 'app directory must be inside /root/matrix',
      details: { app_directory: '/outside' },
    },
  })
})
