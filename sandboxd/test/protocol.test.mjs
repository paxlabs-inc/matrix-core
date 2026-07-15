import test from 'node:test'
import assert from 'node:assert/strict'
import {
  authorized, decodeFiles, parseCurlHeaders, previewSlugFromHost, safeSandboxPath,
} from '../src/protocol.mjs'

test('bearer comparison is exact', () => {
  assert.equal(authorized('Bearer secret', 'secret'), true)
  assert.equal(authorized('Bearer secrex', 'secret'), false)
  assert.equal(authorized('', 'secret'), false)
})

test('wildcard preview host accepts only opaque ids', () => {
  const id = 'a'.repeat(32)
  assert.equal(previewSlugFromHost(`${id}.preview.matrix.test`, 'preview.matrix.test'), id)
  assert.equal(previewSlugFromHost(`alice.preview.matrix.test`, 'preview.matrix.test'), '')
  assert.equal(previewSlugFromHost(`${id}.railway.app`, 'preview.matrix.test'), '')
})

test('file payload stays inside workspace and is bounded', () => {
  assert.equal(safeSandboxPath('src/app.js'), '/workspace/src/app.js')
  assert.throws(() => safeSandboxPath('../secret'))
  const files = decodeFiles([{ path: 'a.txt', content: 'hello' }], 2, 5)
  assert.equal(files[0].content.toString(), 'hello')
  assert.throws(() => decodeFiles([{ path: 'a', content: '123456' }], 2, 5))
})

test('curl response parser keeps application headers and cookies', () => {
  const parsed = parseCurlHeaders('HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nSet-Cookie: a=1\r\nSet-Cookie: b=2\r\nConnection: close\r\n\r\n')
  assert.equal(parsed.status, 200)
  assert.equal(parsed.headers['content-type'], 'text/html')
  assert.deepEqual(parsed.headers['set-cookie'], ['a=1', 'b=2'])
  assert.equal(parsed.headers.connection, undefined)
})
