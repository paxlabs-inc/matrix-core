import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const workspace = mkdtempSync('/tmp/matrix-preview-bridge-')
process.env.MATRIX_WORKSPACE_ROOT = workspace
const { collectFiles, detectLaunch, handleMcpRequest, isDirectExecution } = await import('../../tools/sandbox/sandbox.mjs')

test.after(() => rmSync(workspace, { recursive: true, force: true }))

test('one-call detector fully configures a Vite app', () => {
  const root = join(workspace, 'vite-app')
  mkdirSync(root)
  writeFileSync(join(root, 'package.json'), JSON.stringify({
    scripts: { dev: 'vite' },
    devDependencies: { vite: '^7.0.0' },
  }))
  writeFileSync(join(root, 'package-lock.json'), '{}')
  writeFileSync(join(root, 'index.html'), '<h1>real preview</h1>')

  assert.deepEqual(detectLaunch(root), {
    runtime: 'node',
    framework: 'vite',
    packageManager: 'npm',
    packages: ['nodejs', 'npm'],
    installCommand: 'npm ci --include=dev --include=optional',
    startCommand: 'npm run dev -- --host 0.0.0.0 --port $PORT',
    port: 5173,
    env: { NODE_ENV: 'development' },
    dependencyCheckCommand: 'test -x node_modules/.bin/vite && node_modules/.bin/vite --version',
    requiredBinary: 'vite',
  })
  const upload = collectFiles(root)
  assert.equal(upload.files.length, 3)
  assert.equal(upload.root, root)
})

test('one-call detector serves a static app without agent choices', () => {
  const root = join(workspace, 'static-app')
  mkdirSync(root)
  writeFileSync(join(root, 'index.html'), '<h1>static preview</h1>')
  assert.deepEqual(detectLaunch('static-app'), {
    runtime: 'static',
    framework: 'static',
    packages: ['python3'],
    installCommand: '',
    startCommand: 'python3 -m http.server $PORT --bind 0.0.0.0',
    port: 8080,
  })
})

test('upload bounds fail explicitly instead of silently omitting files', () => {
  const root = join(workspace, 'large-app')
  mkdirSync(root)
  writeFileSync(join(root, 'index.html'), '<h1>large preview</h1>')
  writeFileSync(join(root, 'large.bin'), Buffer.alloc((4 << 20) + 1))
  assert.throws(() => collectFiles(root), (error) => {
    assert.equal(error.code, 'UPLOAD_FILE_TOO_LARGE')
    assert.equal(error.stage, 'upload')
    assert.equal(error.details.file, 'large.bin')
    return true
  })
})

test('Neo sees one tool and receives an exact failure envelope', async () => {
  const advertised = await handleMcpRequest({ jsonrpc: '2.0', id: 1, method: 'tools/list', params: {} })
  assert.deepEqual(advertised.result.tools.map((tool) => tool.name), ['preview_app'])

  const called = await handleMcpRequest({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'preview_app', arguments: { app_directory: '/outside' } } })
  assert.equal(called.result.isError, true)
  assert.deepEqual(JSON.parse(called.result.content[0].text), {
    ok: false,
    status: 'error',
    error: {
      code: 'APP_OUTSIDE_WORKSPACE',
      stage: 'inspect',
      message: `app directory must be inside ${workspace}`,
      details: { app_directory: '/outside' },
    },
  })
})

test('Railway runtime symlink is recognized as direct execution', () => {
  const bridgePath = fileURLToPath(new URL('../../tools/sandbox/sandbox.mjs', import.meta.url))
  const linkedPath = join(workspace, 'sandbox-bridge.mjs')
  symlinkSync(bridgePath, linkedPath)
  assert.equal(isDirectExecution(linkedPath, bridgePath), true)
})
