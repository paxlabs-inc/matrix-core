#!/usr/bin/env node

import { createInterface } from 'node:readline'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { basename, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const SERVER_NAME = 'sandbox'
const SERVER_VERSION = '0.1.0'
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

function clampInt(value, fallback, min, max) {
  const parsed = Number.parseInt(value ?? '', 10)
  return Number.isFinite(parsed) ? Math.min(max, Math.max(min, parsed)) : fallback
}

function result(data, isError = false) {
  return { content: [{ type: 'text', text: JSON.stringify(data) }], isError }
}

function checkedRoot(input) {
  const root = resolve(String(input || ''))
  if (root !== WORKSPACE_ROOT && !root.startsWith(WORKSPACE_ROOT + sep)) throw new Error('project root must be inside the workspace')
  if (!statSync(root).isDirectory()) throw new Error('project root is not a directory')
  return root
}

function collectFiles(rootInput) {
  const root = checkedRoot(rootInput)
  const files = []
  let total = 0
  const visit = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (entry.isSymbolicLink()) continue
      const full = join(dir, entry.name)
      if (entry.isDirectory()) {
        if (!skipDirs.has(entry.name)) visit(full)
        continue
      }
      if (!entry.isFile() || files.length >= MAX_FILES) continue
      const size = statSync(full).size
      if (size > MAX_FILE_BYTES || total + size > MAX_BYTES) continue
      const bytes = readFileSync(full)
      const binary = bytes.subarray(0, 8000).includes(0)
      files.push({
        path: relative(root, full).split(sep).join('/'),
        content: binary ? bytes.toString('base64') : bytes.toString('utf8'),
        ...(binary ? { encoding: 'base64' } : {}),
        ...(statSync(full).mode & 0o111 ? { mode: 0o755 } : {}),
      })
      total += size
    }
  }
  visit(root)
  if (files.length === 0) throw new Error(`project ${basename(root)} has no deployable files`)
  return files
}

function runtimePackages(rootInput) {
  const root = checkedRoot(rootInput)
  const names = new Set(readdirSync(root))
  if (names.has('package.json')) return ['nodejs', 'npm']
  if (names.has('requirements.txt') || names.has('pyproject.toml') || names.has('main.py') || names.has('app.py')) return ['python3', 'python3-pip', 'python3-venv']
  if (names.has('go.mod')) return ['golang-go']
  if (names.has('Cargo.toml')) return ['cargo', 'rustc']
  return []
}

async function request(method, path, body) {
  if (!REMOTE_URL) throw new Error('sandbox service is not configured')
  if (!TOKEN || !USER_ID) throw new Error('sandbox service identity is not configured')
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
    let value = null
    try { value = raw ? JSON.parse(raw) : {} } catch { value = { error: 'sandbox service returned an unreadable response' } }
    if (!response.ok) throw new Error(value?.error || 'sandbox service rejected the operation')
    return value
  } catch (error) {
    if (error?.name === 'AbortError') throw new Error('sandbox operation timed out')
    throw error
  } finally {
    clearTimeout(timer)
  }
}

async function call(name, args) {
  switch (name) {
    case 'sandbox_create':
      return request('POST', '/v1/sandboxes', {
        files: collectFiles(args.root),
        start_command: args.start_command,
        install_command: args.install_command,
        port: args.port,
        ttl_seconds: args.ttl_seconds,
        packages: args.packages || runtimePackages(args.root),
        env: args.env,
      })
    case 'sandbox_list':
      return request('GET', '/v1/sandboxes')
    case 'sandbox_exec':
      return request('POST', `/v1/sandboxes/${encodeURIComponent(args.id)}/exec`, {
        command: args.command,
        timeout_seconds: args.timeout_seconds,
      })
    case 'sandbox_sync':
      return request('PUT', `/v1/sandboxes/${encodeURIComponent(args.id)}/files`, { files: collectFiles(args.root) })
    case 'sandbox_destroy':
      return request('DELETE', `/v1/sandboxes/${encodeURIComponent(args.id)}`)
    default:
      throw new Error(`unknown tool ${name}`)
  }
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
    if (!toolSet.has(name)) return result({ ok: false, error: `unknown tool ${name}` }, true)
    try { return result({ ok: true, data: await call(name, params?.arguments || {}) }) }
    catch (error) { return result({ ok: false, error: error?.message || String(error) }, true) }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(value) { process.stdout.write(JSON.stringify(value) + '\n') }

function start() {
  const input = createInterface({ input: process.stdin })
  input.on('line', async (line) => {
    if (!line.trim()) return
    let request
    try { request = JSON.parse(line) }
    catch { return send({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } }) }
    const handler = handlers[request.method]
    if (!handler) {
      if (request.id !== undefined) send({ jsonrpc: '2.0', id: request.id, error: { code: -32601, message: 'method not found' } })
      return
    }
    try {
      const value = await handler(request.params)
      if (request.id !== undefined && value !== null) send({ jsonrpc: '2.0', id: request.id, result: value })
    } catch (error) {
      if (request.id !== undefined) send({ jsonrpc: '2.0', id: request.id, error: { code: -32000, message: error?.message || String(error) } })
    }
  })
}

if (process.argv.includes('--selftest')) {
  console.log(`sandbox: ${tools.length} tools`)
  console.log('sandbox OK')
} else {
  start()
}
