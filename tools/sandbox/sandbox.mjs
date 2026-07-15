#!/usr/bin/env node

import { createInterface } from 'node:readline'
import { readFileSync, readdirSync, realpathSync, statSync } from 'node:fs'
import { basename, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const SERVER_NAME = 'sandbox'
const SERVER_VERSION = '0.2.0'
const PROTOCOL_VERSION = '2024-11-05'
const REMOTE_URL = String(process.env.MATRIX_SANDBOX_URL || '').trim().replace(/\/+$/, '')
const TOKEN = String(process.env.MATRIX_SANDBOX_TOKEN || '').trim()
const USER_ID = String(process.env.MATRIX_USER_ID || '').trim()
const WORKSPACE_ROOT = resolve(process.env.NEO_WORKSPACE_DIR || process.env.MATRIX_WORKSPACE_ROOT || '/workspace')
const TIMEOUT_MS = clampInt(process.env.MATRIX_SANDBOX_TIMEOUT_MS, 900000, 2000, 900000)
const MAX_FILES = 6000
const MAX_BYTES = 48 << 20
const MAX_FILE_BYTES = 4 << 20
const skipDirs = new Set(['.git', 'node_modules', 'vendor', 'dist', 'build', 'target', '.next', '__pycache__', '.venv', 'venv', '.cache', '.turbo', '.neo'])

const tools = JSON.parse(readFileSync(fileURLToPath(new URL('./sandbox-tools.json', import.meta.url)), 'utf8'))
const toolSet = new Set(tools.map((tool) => tool.name))

class PreviewFailure extends Error {
  constructor(code, stage, message, details = {}) {
    super(message)
    this.code = code
    this.stage = stage
    this.details = details
  }
}

function clampInt(value, fallback, min, max) {
  const parsed = Number.parseInt(value ?? '', 10)
  return Number.isFinite(parsed) ? Math.min(max, Math.max(min, parsed)) : fallback
}

function result(data, isError = false) {
  return { content: [{ type: 'text', text: JSON.stringify(data) }], isError }
}

function fail(code, stage, message, details) {
  throw new PreviewFailure(code, stage, message, details)
}

function errorEnvelope(error, fallbackStage = 'preview') {
  return {
    ok: false,
    status: 'error',
    error: {
      code: error?.code || 'PREVIEW_FAILED',
      stage: error?.stage || fallbackStage,
      message: error?.message || String(error),
      ...(error?.details && Object.keys(error.details).length ? { details: error.details } : {}),
    },
  }
}

function checkedRoot(input) {
  const value = String(input || '').trim()
  if (!value) fail('APP_DIRECTORY_REQUIRED', 'inspect', 'app_directory is required')
  const root = resolve(value.startsWith('/') ? value : join(WORKSPACE_ROOT, value))
  if (root !== WORKSPACE_ROOT && !root.startsWith(WORKSPACE_ROOT + sep)) {
    fail('APP_OUTSIDE_WORKSPACE', 'inspect', `app directory must be inside ${WORKSPACE_ROOT}`, { app_directory: value })
  }
  let stat
  try { stat = statSync(root) } catch {
    fail('APP_DIRECTORY_NOT_FOUND', 'inspect', `app directory does not exist: ${root}`, { app_directory: root })
  }
  if (!stat.isDirectory()) fail('APP_DIRECTORY_INVALID', 'inspect', `app path is not a directory: ${root}`, { app_directory: root })
  return root
}

export function collectFiles(rootInput) {
  const root = checkedRoot(rootInput)
  const files = []
  const skippedSymlinks = []
  let total = 0
  const visit = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = join(dir, entry.name)
      const path = relative(root, full).split(sep).join('/')
      if (entry.isSymbolicLink()) {
        skippedSymlinks.push(path)
        continue
      }
      if (entry.isDirectory()) {
        if (!skipDirs.has(entry.name)) visit(full)
        continue
      }
      if (!entry.isFile()) continue
      if (files.length >= MAX_FILES) {
        fail('UPLOAD_FILE_LIMIT', 'upload', `project contains more than ${MAX_FILES} uploadable files`, { app_directory: root, first_omitted_file: path })
      }
      const stat = statSync(full)
      if (stat.size > MAX_FILE_BYTES) {
        fail('UPLOAD_FILE_TOO_LARGE', 'upload', `file exceeds the ${MAX_FILE_BYTES}-byte per-file limit: ${path}`, { file: path, bytes: stat.size, limit_bytes: MAX_FILE_BYTES })
      }
      if (total + stat.size > MAX_BYTES) {
        fail('UPLOAD_TOTAL_TOO_LARGE', 'upload', `project exceeds the ${MAX_BYTES}-byte upload limit`, { first_omitted_file: path, bytes_before_file: total, limit_bytes: MAX_BYTES })
      }
      const bytes = readFileSync(full)
      files.push({
        path,
        content: bytes.toString('base64'),
        encoding: 'base64',
        ...(stat.mode & 0o111 ? { mode: 0o755 } : {}),
      })
      total += stat.size
    }
  }
  visit(root)
  if (files.length === 0) fail('APP_EMPTY', 'inspect', `app directory has no deployable files: ${root}`, { app_directory: root })
  return { root, files, bytes: total, skippedSymlinks }
}

function namesIn(root) {
  return new Set(readdirSync(root))
}

function optionalText(root, name) {
  try { return readFileSync(join(root, name), 'utf8') } catch { return '' }
}

function explicitStart(root) {
  const procfile = optionalText(root, 'Procfile').split(/\r?\n/).find((line) => /^web\s*:/.test(line))
  if (procfile) return procfile.replace(/^web\s*:\s*/, '').trim()
  try {
    const railway = JSON.parse(optionalText(root, 'railway.json'))
    if (String(railway?.deploy?.startCommand || '').trim()) return String(railway.deploy.startCommand).trim()
  } catch {}
  const toml = optionalText(root, 'railway.toml').match(/^\s*startCommand\s*=\s*["'](.+)["']\s*$/m)
  return toml?.[1]?.trim() || ''
}

function packageManager(root, manifest, names) {
  const declared = String(manifest.packageManager || '')
  if (names.has('pnpm-lock.yaml') || declared.startsWith('pnpm@')) {
    const version = /^pnpm@([0-9][0-9A-Za-z.-]*)$/.exec(declared)?.[1] || 'latest'
    return {
      name: 'pnpm',
      install: `npm install --global pnpm@${version} && pnpm install --frozen-lockfile`,
      run: (script) => `pnpm run ${script}`,
    }
  }
  if (names.has('yarn.lock') || declared.startsWith('yarn@')) {
    const version = /^yarn@([0-9][0-9A-Za-z.-]*)$/.exec(declared)?.[1] || 'latest'
    return {
      name: 'yarn',
      install: `npm install --global yarn@${version} && yarn install --frozen-lockfile`,
      run: (script) => `yarn run ${script}`,
    }
  }
  return {
    name: 'npm',
    install: names.has('package-lock.json') || names.has('npm-shrinkwrap.json') ? 'npm ci' : 'npm install',
    run: (script) => `npm run ${script}`,
  }
}

function frameworkFor(manifest, script) {
  const dependencies = { ...manifest.dependencies, ...manifest.devDependencies }
  const source = `${script} ${Object.keys(dependencies).join(' ')}`.toLowerCase()
  if (/\bnext\b/.test(source)) return 'next'
  if (/\b(vite|svelte-kit|astro|nuxt|remix)\b/.test(source)) return /\bastro\b/.test(source) ? 'astro' : /\bnuxt\b/.test(source) ? 'nuxt' : /\bsvelte-kit\b/.test(source) ? 'sveltekit' : /\bremix\b/.test(source) ? 'remix' : 'vite'
  if (/react-scripts/.test(source)) return 'create-react-app'
  return 'node'
}

function nodeLaunch(root, names) {
  let manifest
  try { manifest = JSON.parse(optionalText(root, 'package.json')) } catch (error) {
    fail('PACKAGE_JSON_INVALID', 'inspect', `package.json is not valid JSON: ${error.message}`, { app_directory: root })
  }
  const scripts = manifest?.scripts || {}
  const manager = packageManager(root, manifest, names)
  const explicit = explicitStart(root)
  let selected = explicit ? 'configured' : scripts.dev ? 'dev' : scripts.start ? 'start' : scripts.preview ? 'preview' : ''
  if (!selected) {
    fail('START_SCRIPT_NOT_FOUND', 'inspect', 'package.json must define a dev, start, or preview script, or the app must provide Procfile/railway startCommand', { available_scripts: Object.keys(scripts) })
  }
  const scriptBody = explicit || scripts[selected] || ''
  const framework = frameworkFor(manifest, scriptBody)
  let startCommand = explicit || manager.run(selected)
  if (!explicit && ['vite', 'astro', 'nuxt', 'sveltekit', 'remix'].includes(framework)) startCommand += ' -- --host 0.0.0.0 --port $PORT'
  if (!explicit && framework === 'next') startCommand += ' -- --hostname 0.0.0.0 --port $PORT'
  let installCommand = manager.install
  if (!explicit && selected !== 'dev' && scripts.build) installCommand += ` && ${manager.run('build')}`
  const ports = { vite: 5173, astro: 4321, nuxt: 3000, sveltekit: 5173, remix: 5173, next: 3000 }
  return {
    runtime: 'node', framework, packageManager: manager.name,
    packages: ['nodejs', 'npm'], installCommand, startCommand, port: ports[framework] || 3000,
  }
}

function pythonLaunch(root, names) {
  const dependencyText = `${optionalText(root, 'requirements.txt')}\n${optionalText(root, 'pyproject.toml')}`.toLowerCase()
  const venv = '/workspace/.matrix-venv/bin'
  let installCommand = `python3 -m venv /workspace/.matrix-venv`
  if (names.has('requirements.txt')) installCommand += ` && ${venv}/pip install -r requirements.txt`
  else if (names.has('pyproject.toml')) installCommand += ` && ${venv}/pip install .`
  const explicit = explicitStart(root)
  if (explicit) return { runtime: 'python', framework: 'configured', packages: ['python3', 'python3-pip', 'python3-venv'], installCommand, startCommand: explicit, port: 8000 }
  if (names.has('manage.py')) return { runtime: 'python', framework: 'django', packages: ['python3', 'python3-pip', 'python3-venv'], installCommand, startCommand: `${venv}/python manage.py runserver 0.0.0.0:$PORT`, port: 8000 }
  const module = names.has('main.py') ? 'main' : names.has('app.py') ? 'app' : names.has('server.py') ? 'server' : ''
  if (/streamlit/.test(dependencyText) && module) return { runtime: 'python', framework: 'streamlit', packages: ['python3', 'python3-pip', 'python3-venv'], installCommand, startCommand: `${venv}/streamlit run ${module}.py --server.address 0.0.0.0 --server.port $PORT`, port: 8501 }
  if (/uvicorn|fastapi/.test(dependencyText) && module) return { runtime: 'python', framework: 'asgi', packages: ['python3', 'python3-pip', 'python3-venv'], installCommand, startCommand: `${venv}/uvicorn ${module}:app --host 0.0.0.0 --port $PORT`, port: 8000 }
  if (/flask/.test(dependencyText) && module) return { runtime: 'python', framework: 'flask', packages: ['python3', 'python3-pip', 'python3-venv'], installCommand, startCommand: `${venv}/flask --app ${module}:app run --host 0.0.0.0 --port $PORT`, port: 5000 }
  if (module) return { runtime: 'python', framework: 'python', packages: ['python3', 'python3-pip', 'python3-venv'], installCommand, startCommand: `${venv}/python ${module}.py`, port: 8000 }
  fail('PYTHON_ENTRYPOINT_NOT_FOUND', 'inspect', 'Python app requires manage.py, main.py, app.py, server.py, Procfile, or railway startCommand', { app_directory: root })
}

export function detectLaunch(rootInput) {
  const root = checkedRoot(rootInput)
  const names = namesIn(root)
  if (names.has('package.json')) return nodeLaunch(root, names)
  if (names.has('requirements.txt') || names.has('pyproject.toml') || names.has('main.py') || names.has('app.py') || names.has('server.py') || names.has('manage.py')) return pythonLaunch(root, names)
  if (names.has('go.mod')) return { runtime: 'go', framework: 'go', packages: ['golang-go'], installCommand: 'go mod download', startCommand: explicitStart(root) || 'go run .', port: 8080 }
  if (names.has('Cargo.toml')) return { runtime: 'rust', framework: 'rust', packages: ['cargo', 'rustc'], installCommand: 'cargo fetch', startCommand: explicitStart(root) || 'cargo run --release', port: 8080 }
  if (names.has('index.html')) return { runtime: 'static', framework: 'static', packages: ['python3'], installCommand: '', startCommand: 'python3 -m http.server $PORT --bind 0.0.0.0', port: 8080 }
  fail('APP_RUNTIME_UNSUPPORTED', 'inspect', 'could not detect a supported web app runtime', { app_directory: root, expected: ['package.json', 'requirements.txt or pyproject.toml', 'go.mod', 'Cargo.toml', 'index.html'] })
}

async function request(method, path, body) {
  if (!REMOTE_URL) fail('SERVICE_NOT_CONFIGURED', 'connect', 'sandbox service URL is not configured')
  if (!TOKEN || !USER_ID) fail('IDENTITY_NOT_CONFIGURED', 'connect', 'sandbox service token or Matrix user identity is not configured')
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  try {
    const response = await fetch(`${REMOTE_URL}${path}`, {
      method,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        Authorization: `Bearer ${TOKEN}`,
        'X-Matrix-User': USER_ID,
      },
      body: body == null ? undefined : JSON.stringify(body),
      signal: controller.signal,
    })
    const raw = await response.text()
    let value
    try { value = raw ? JSON.parse(raw) : {} } catch {
      fail('SERVICE_RESPONSE_INVALID', 'connect', `sandbox service returned non-JSON HTTP ${response.status}`, { http_status: response.status, response: raw.slice(0, 1000) })
    }
    if (!response.ok) {
      const remote = typeof value?.error === 'object' ? value.error : null
      fail(remote?.code || 'SERVICE_REJECTED', remote?.stage || 'provision', remote?.message || value?.error || `sandbox service rejected the request with HTTP ${response.status}`, { http_status: response.status, ...(remote?.details || {}) })
    }
    return value
  } catch (error) {
    if (error instanceof PreviewFailure) throw error
    if (error?.name === 'AbortError') fail('SANDBOX_TIMEOUT', 'provision', `sandbox operation exceeded ${TIMEOUT_MS}ms`, { timeout_ms: TIMEOUT_MS })
    fail('SERVICE_UNREACHABLE', 'connect', `could not reach sandbox service: ${error?.message || error}`)
  } finally {
    clearTimeout(timer)
  }
}

async function verifyPreview(url) {
  let last = 'no response'
  for (let attempt = 1; attempt <= 20; attempt++) {
    try {
      const response = await fetch(url, { redirect: 'follow', signal: AbortSignal.timeout(10_000) })
      const reader = response.body?.getReader()
      const first = reader ? await reader.read() : { value: new Uint8Array() }
      await reader?.cancel()
      if (response.status >= 200 && response.status < 400) {
        return { http_status: response.status, content_type: response.headers.get('content-type') || '', first_response_bytes: first.value?.byteLength || 0, attempts: attempt }
      }
      last = `HTTP ${response.status}`
    } catch (error) {
      last = error?.message || String(error)
    }
    if (attempt < 20) await new Promise((resolve) => setTimeout(resolve, 3000))
  }
  fail('PUBLIC_PREVIEW_UNREACHABLE', 'verify', `public preview did not become reachable: ${last}`, { preview_url: url, attempts: 20, last_error: last })
}

async function previewApp(args) {
  const upload = collectFiles(args.app_directory)
  const launch = detectLaunch(upload.root)
  let created
  try {
    created = await request('POST', '/v1/sandboxes', {
      files: upload.files,
      start_command: launch.startCommand,
      install_command: launch.installCommand,
      port: launch.port,
      ttl_seconds: 1800,
      packages: launch.packages,
    })
    const verification = await verifyPreview(created.preview_url)
    return {
      ok: true,
      status: 'success',
      message: `Preview is live at ${created.preview_url}`,
      preview_url: created.preview_url,
      sandbox_id: created.id,
      app_directory: upload.root,
      runtime: launch.runtime,
      framework: launch.framework,
      port: launch.port,
      install_command: launch.installCommand || null,
      start_command: launch.startCommand,
      upload: { files: upload.files.length, bytes: upload.bytes, skipped_symlinks: upload.skippedSymlinks },
      verification,
      expires_at: created.expires_at,
    }
  } catch (error) {
    if (created?.id) {
      try { await request('DELETE', `/v1/sandboxes/${encodeURIComponent(created.id)}`) }
      catch (cleanupError) {
        error.details = { ...(error.details || {}), sandbox_id: created.id, cleanup_error: cleanupError.message }
      }
    }
    throw error
  }
}

async function call(name, args) {
  if (name === 'preview_app') return previewApp(args)
  fail('TOOL_UNKNOWN', 'request', `unknown tool ${name}`)
}

const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? PROTOCOL_VERSION,
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    if (!toolSet.has(name)) return result(errorEnvelope(new PreviewFailure('TOOL_UNKNOWN', 'request', `unknown tool ${name}`)), true)
    try { return result(await call(name, params?.arguments || {})) }
    catch (error) { return result(errorEnvelope(error), true) }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(value) { process.stdout.write(JSON.stringify(value) + '\n') }

let inputInterface

export async function handleMcpRequest(requestValue) {
  const handler = handlers[requestValue.method]
  if (!handler) {
    return requestValue.id === undefined ? null : { jsonrpc: '2.0', id: requestValue.id, error: { code: -32601, message: 'method not found' } }
  }
  try {
    const value = await handler(requestValue.params)
    return requestValue.id !== undefined && value !== null ? { jsonrpc: '2.0', id: requestValue.id, result: value } : null
  } catch (error) {
    return requestValue.id === undefined ? null : { jsonrpc: '2.0', id: requestValue.id, error: { code: -32000, message: error?.message || String(error) } }
  }
}

function start() {
  process.stdin.resume()
  inputInterface = createInterface({ input: process.stdin })
  inputInterface.on('line', async (line) => {
    if (!line.trim()) return
    let requestValue
    try { requestValue = JSON.parse(line) }
    catch { return send({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } }) }
    const response = await handleMcpRequest(requestValue)
    if (response) send(response)
  })
}

export function isDirectExecution(executablePath = process.argv[1], modulePath = fileURLToPath(import.meta.url)) {
  if (!executablePath) return false
  try { return realpathSync(executablePath) === realpathSync(modulePath) }
  catch { return pathToFileURL(modulePath).href === pathToFileURL(executablePath).href }
}

const direct = isDirectExecution()
if (direct && process.argv.includes('--selftest')) {
  console.log(`sandbox: ${tools.length} tools`)
  console.log('sandbox OK')
} else if (direct) {
  start()
}
