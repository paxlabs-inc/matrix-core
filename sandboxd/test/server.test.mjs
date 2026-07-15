import test from 'node:test'
import assert from 'node:assert/strict'
import { once } from 'node:events'
import { spawn } from 'node:child_process'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import { SandboxManager } from '../src/manager.mjs'
import { createServer } from '../src/server.mjs'

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
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => server.close())
  const base = `http://127.0.0.1:${server.address().port}`

  const health = await fetch(`${base}/healthz`)
  assert.equal(health.status, 200)

  const denied = await fetch(`${base}/v1/sandboxes`)
  assert.equal(denied.status, 401)

  const listed = await fetch(`${base}/v1/sandboxes`, {
    headers: { Authorization: 'Bearer transport-secret', 'X-Matrix-User': 'alice' },
  })
  assert.equal(listed.status, 200)
  const body = await listed.json()
  assert.equal(body.sandboxes.length, 1)
  assert.equal(body.sandboxes[0].preview_url, `https://${'a'.repeat(32)}.preview.matrix.test/`)

  const other = await fetch(`${base}/v1/sandboxes`, {
    headers: { Authorization: 'Bearer transport-secret', 'X-Matrix-User': 'bob' },
  })
  assert.deepEqual(await other.json(), { sandboxes: [] })

  const bridge = spawn(process.execPath, [fileURLToPath(new URL('../../tools/sandbox/sandbox.mjs', import.meta.url))], {
    env: {
      ...process.env,
      MATRIX_SANDBOX_URL: base,
      MATRIX_SANDBOX_TOKEN: 'transport-secret',
      MATRIX_USER_ID: 'alice',
    },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  t.after(() => bridge.kill())
  const lines = createInterface({ input: bridge.stdout })[Symbol.asyncIterator]()
  bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list', params: {} }) + '\n')
  const advertised = JSON.parse((await lines.next()).value)
  assert.deepEqual(advertised.result.tools.map((tool) => tool.name), [
    'sandbox_create', 'sandbox_list', 'sandbox_exec', 'sandbox_sync', 'sandbox_destroy',
  ])
  bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'sandbox_list', arguments: {} } }) + '\n')
  const called = JSON.parse((await lines.next()).value)
  const envelope = JSON.parse(called.result.content[0].text)
  assert.equal(envelope.ok, true)
  assert.equal(envelope.data.sandboxes[0].id, 'a'.repeat(32))
})
