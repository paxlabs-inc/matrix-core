#!/usr/bin/env node

// desktop.mjs — the DOJO desktop actuation bridge (DOJO wave 2, req 2). A thin
// stdio-MCP façade over the bytebotd /computer-use contract, following the
// house remote-service bridge pattern (protocol/tools/sandbox/sandbox.mjs is the
// precedent). Every tool maps one-to-one onto a bytebotd action and is POSTed
// to MATRIX_DOJO_URL — in production the neo daemon's /dojo/computer-use proxy,
// which resolves the user's live disposable-desktop session and rides
// sandboxExec-curl into the appliance's localhost:9990 (the sandbox has no
// privateHost, wave-1). The bridge holds NO Railway token and NO session id:
// the daemon owns the session, the transport, and the guard chain.

import { createInterface } from 'node:readline'
import { readFileSync, realpathSync } from 'node:fs'
import { fileURLToPath, pathToFileURL } from 'node:url'

const SERVER_NAME = 'desktop'
const SERVER_VERSION = '0.1.0'
const PROTOCOL_VERSION = '2024-11-05'
const REMOTE_URL = String(process.env.MATRIX_DOJO_URL || '').trim().replace(/\/+$/, '')
const TOKEN = String(process.env.MATRIX_DOJO_TOKEN || '').trim()
const USER_ID = String(process.env.MATRIX_USER_ID || '').trim()
const TIMEOUT_MS = clampInt(process.env.MATRIX_DOJO_TIMEOUT_MS, 60000, 2000, 180000)

const tools = JSON.parse(readFileSync(fileURLToPath(new URL('./desktop-tools.json', import.meta.url)), 'utf8'))
const toolSet = new Set(tools.map((tool) => tool.name))

// bytebotAction maps a desktop_* tool call to the bytebotd /computer-use
// request body. Each returns { action, ... } exactly as bytebotd expects.
const bytebotAction = {
  desktop_move_mouse: (a) => ({ action: 'move_mouse', coordinates: xy(a) }),
  desktop_click_mouse: (a) => ({ action: 'click_mouse', coordinates: xy(a), button: a.button || 'left', clickCount: Number(a.clickCount) >= 1 ? Number(a.clickCount) : 1 }),
  desktop_press_mouse: (a) => ({ action: 'press_mouse', coordinates: xy(a), press: a.press, button: a.button || 'left' }),
  desktop_drag_mouse: (a) => ({ action: 'drag_mouse', path: (a.path || []).map(xy), button: a.button || 'left' }),
  desktop_scroll: (a) => trim({ action: 'scroll', coordinates: xy(a), direction: a.direction, scrollCount: a.scrollCount }),
  desktop_type_text: (a) => trim({ action: 'type_text', text: a.text, delay: a.delay }),
  desktop_paste_text: (a) => ({ action: 'paste_text', text: a.text }),
  desktop_type_keys: (a) => ({ action: 'type_keys', keys: a.keys || [] }),
  desktop_screenshot: () => ({ action: 'screenshot' }),
  desktop_cursor_position: () => ({ action: 'cursor_position' }),
  desktop_wait: (a) => ({ action: 'wait', duration: a.ms }),
  desktop_application: (a) => ({ action: 'application', application: a.application }),
  desktop_read_file: (a) => ({ action: 'read_file', path: a.path }),
  desktop_write_file: (a) => ({ action: 'write_file', path: a.path, data: a.data }),
}

function clampInt(value, fallback, min, max) {
  const parsed = Number.parseInt(value ?? '', 10)
  return Number.isFinite(parsed) ? Math.min(max, Math.max(min, parsed)) : fallback
}

function xy(a) {
  return { x: Number(a.x) | 0, y: Number(a.y) | 0 }
}

// trim drops undefined keys so an optional field the model omitted never rides
// the wire as null.
function trim(obj) {
  for (const k of Object.keys(obj)) if (obj[k] === undefined || obj[k] === null) delete obj[k]
  return obj
}

function textResult(data, isError = false) {
  return { content: [{ type: 'text', text: typeof data === 'string' ? data : JSON.stringify(data) }], isError }
}

function imageResult(base64, mimeType = 'image/png') {
  // Mirrors the browser bridge: an MCP image content block the Go DispatchMedia
  // path persists out-of-band; the model-facing text stays a terse placeholder.
  return {
    content: [
      { type: 'image', data: base64, mimeType },
      { type: 'text', text: 'Desktop screenshot captured (shown to the user).' },
    ],
  }
}

async function callDaemon(body) {
  if (!REMOTE_URL) return { error: { code: 'DESKTOP_NOT_CONFIGURED', message: 'no disposable desktop is configured in this environment (MATRIX_DOJO_URL unset)' } }
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  try {
    const headers = { Accept: 'application/json', 'Content-Type': 'application/json' }
    if (TOKEN) headers.Authorization = `Bearer ${TOKEN}`
    if (USER_ID) headers['X-Matrix-User'] = USER_ID
    const response = await fetch(`${REMOTE_URL}/computer-use`, {
      method: 'POST',
      headers,
      body: JSON.stringify(body),
      signal: controller.signal,
    })
    const raw = await response.text()
    let value
    try { value = raw ? JSON.parse(raw) : {} } catch {
      return { error: { code: 'DESKTOP_RESPONSE_INVALID', message: `desktop returned non-JSON HTTP ${response.status}`, http_status: response.status, body: raw.slice(0, 500) } }
    }
    if (!response.ok) {
      return { error: { code: 'DESKTOP_REJECTED', message: value?.message || value?.error || `desktop rejected the action with HTTP ${response.status}`, http_status: response.status } }
    }
    return { value }
  } catch (error) {
    if (error?.name === 'AbortError') return { error: { code: 'DESKTOP_TIMEOUT', message: `desktop action exceeded ${TIMEOUT_MS}ms` } }
    return { error: { code: 'DESKTOP_UNREACHABLE', message: `could not reach the desktop: ${error?.message || error}` } }
  } finally {
    clearTimeout(timer)
  }
}

async function call(name, args) {
  const build = bytebotAction[name]
  if (!build) return textResult({ error: { code: 'TOOL_UNKNOWN', message: `unknown tool ${name}` } }, true)
  const body = build(args || {})
  const { value, error } = await callDaemon(body)
  if (error) return textResult({ ok: false, ...error && { error } }, true)
  // Screenshot comes back as { image: <base64 png> } — surface it as an MCP
  // image so it reaches the user out-of-band, exactly like browser screenshots.
  if (name === 'desktop_screenshot' && value?.image) return imageResult(value.image)
  return textResult({ ok: true, ...value })
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
    if (!toolSet.has(name)) return textResult({ error: { code: 'TOOL_UNKNOWN', message: `unknown tool ${name}` } }, true)
    return call(name, params?.arguments || {})
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
  const names = tools.map((t) => t.name)
  const missing = names.filter((n) => !bytebotAction[n])
  const extra = Object.keys(bytebotAction).filter((n) => !toolSet.has(n))
  if (missing.length || extra.length) {
    console.error(`desktop: registry/impl mismatch missing=${missing} extra=${extra}`)
    process.exit(1)
  }
  console.log(`desktop: ${tools.length} tools`)
  console.log('desktop OK')
} else if (direct) {
  start()
}
