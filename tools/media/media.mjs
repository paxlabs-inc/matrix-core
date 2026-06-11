#!/usr/bin/env node
// media — MCP stdio bridge giving Matrix agents (Neo + the MCL daemon) media
// I/O: generate/edit images, generate video, and transcribe audio via the
// Together AI media APIs.
//
// Why a local bridge (not a remote MCP)? Together exposes plain REST media
// endpoints, not an MCP server. This bridge speaks the daemon's stdio JSON-RPC
// (executor/mcp/stdio) on one side and calls Together on the other, shaping
// each result into a CallToolResult. It mirrors tools/paxeer/paxeer-net.mjs.
//
// Storage: generated/edited outputs are written to MATRIX_MEDIA_DIR — the
// agent's OWN machine volume (e.g. /data/media), the same volume that holds
// cortex + executor.key — and the tool returns a relative {url} under
// MATRIX_MEDIA_BASE (default /media) that the Neo server streams back to the
// browser. Input refs (attachments / prior outputs) are resolved off the SAME
// volume and inlined to Together as base64 data URIs, so nothing private is
// ever publicly exposed.
//
// Auth: TOGETHER_API_KEY (Bearer). Without it the media_* tools return a
// structured error rather than bricking daemon boot (spawn stays non-fatal).
//
// Models (env-overridable):
//   MATRIX_MEDIA_IMAGE_MODEL  default black-forest-labs/FLUX.1-kontext-pro
//   MATRIX_MEDIA_VIDEO_MODEL  default ByteDance/Seedance-2.0
//   MATRIX_MEDIA_AUDIO_MODEL  default openai/whisper-large-v3
//
// Run `node tools/media/media.mjs --selftest` to smoke it offline.

import { createInterface } from 'node:readline'
import { readdirSync, readFileSync, mkdirSync } from 'node:fs'
import { readFile, writeFile } from 'node:fs/promises'
import { randomBytes } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { join, basename } from 'node:path'
import { tmpdir } from 'node:os'

const SERVER_NAME = 'media'
const SERVER_VERSION = '0.1.0'
const PROTOCOL_VERSION = '2024-11-05'

const API_BASE = (process.env.MATRIX_MEDIA_API_BASE || 'https://api.together.xyz').replace(/\/+$/, '')
const API_KEY = (process.env.TOGETHER_API_KEY || '').trim()
const IMAGE_MODEL = (process.env.MATRIX_MEDIA_IMAGE_MODEL || 'black-forest-labs/FLUX.1-kontext-pro').trim()
const VIDEO_MODEL = (process.env.MATRIX_MEDIA_VIDEO_MODEL || 'ByteDance/Seedance-2.0').trim()
const AUDIO_MODEL = (process.env.MATRIX_MEDIA_AUDIO_MODEL || 'openai/whisper-large-v3').trim()

const MEDIA_DIR = (process.env.MATRIX_MEDIA_DIR || join(tmpdir(), 'matrix-media')).replace(/\/+$/, '')
const MEDIA_BASE = '/' + (process.env.MATRIX_MEDIA_BASE || '/media').replace(/^\/+|\/+$/g, '')

const HTTP_TIMEOUT_MS = clampInt(process.env.MATRIX_MEDIA_TIMEOUT_MS, 120000, 5000, 300000)
const VIDEO_POLL_MS = clampInt(process.env.MATRIX_MEDIA_VIDEO_POLL_MS, 6000, 1000, 60000)
const VIDEO_MAX_WAIT_MS = clampInt(process.env.MATRIX_MEDIA_VIDEO_MAX_WAIT_MS, 540000, 30000, 1800000)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

// ── static tool registry (advertised verbatim; must equal the manifest) ──────
const TOOLS_PATH = fileURLToPath(new URL('./media-tools.json', import.meta.url))
let tools = []
try {
  tools = JSON.parse(readFileSync(TOOLS_PATH, 'utf8'))
} catch (err) {
  console.error(`media: cannot load tool registry ${TOOLS_PATH}: ${err.message}`)
  process.exit(1)
}
const TOOL_NAMES = tools.map((t) => t.name)
const TOOL_SET = new Set(TOOL_NAMES)

// ── result shaping ───────────────────────────────────────────────────────────
function okResult(obj) {
  return { content: [{ type: 'text', text: JSON.stringify(obj) }], isError: false }
}
function errResult(tool, error, extra = {}) {
  return {
    content: [{ type: 'text', text: JSON.stringify({ ok: false, tool, error, ...extra }) }],
    isError: true,
  }
}

// ── media files (write outputs / resolve local refs on the shared volume) ────
const EXT_MIME = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', webp: 'image/webp', gif: 'image/gif',
  mp4: 'video/mp4', webm: 'video/webm', mov: 'video/quicktime',
  mp3: 'audio/mpeg', wav: 'audio/wav', m4a: 'audio/mp4', flac: 'audio/flac',
  ogg: 'audio/ogg', opus: 'audio/opus', aac: 'audio/aac',
}

function mimeForExt(ext) {
  return EXT_MIME[String(ext).toLowerCase()] || 'application/octet-stream'
}

// Sniff a small set of image/video magic numbers so the written extension and
// served MIME match the actual bytes Together returns.
function sniffImage(buf) {
  if (buf.length >= 4 && buf[0] === 0x89 && buf[1] === 0x50 && buf[2] === 0x4e && buf[3] === 0x47) return { ext: 'png', mime: 'image/png' }
  if (buf.length >= 3 && buf[0] === 0xff && buf[1] === 0xd8 && buf[2] === 0xff) return { ext: 'jpg', mime: 'image/jpeg' }
  if (buf.length >= 12 && buf.toString('ascii', 8, 12) === 'WEBP') return { ext: 'webp', mime: 'image/webp' }
  return { ext: 'png', mime: 'image/png' }
}

function mintId() {
  return Date.now().toString(36) + randomBytes(6).toString('hex')
}

async function writeOutput(buf, ext, mime) {
  mkdirSync(MEDIA_DIR, { recursive: true })
  const id = mintId()
  const name = `${id}.${ext}`
  await writeFile(join(MEDIA_DIR, name), buf)
  return { url: `${MEDIA_BASE}/${name}`, mime, bytes: buf.length, file: name }
}

// localName maps a /media/<name> ref (or bare <name>) to a safe filename in
// MEDIA_DIR, or null if the ref is not a local media reference.
function localName(ref) {
  const s = String(ref || '').trim()
  if (!s) return null
  if (/^https?:\/\//i.test(s)) return null
  if (/^data:/i.test(s)) return null
  let name = s
  if (name.startsWith(MEDIA_BASE + '/')) name = name.slice(MEDIA_BASE.length + 1)
  name = name.replace(/^\/+/, '')
  // Single path segment only — no traversal, no nested dirs.
  if (name !== basename(name) || name === '' || name.includes('..')) return null
  return name
}

function extOf(name) {
  const i = name.lastIndexOf('.')
  return i >= 0 ? name.slice(i + 1).toLowerCase() : ''
}

// resolveImageInput turns an image ref into something Together's image_url
// accepts: a passed-through public URL, or a base64 data URI read off the
// local volume (keeps private attachments off the public internet).
async function resolveImageInput(ref) {
  const s = String(ref || '').trim()
  if (/^https?:\/\//i.test(s) || /^data:/i.test(s)) return s
  const name = localName(s)
  if (!name) throw new Error(`cannot resolve image reference: ${ref}`)
  const buf = await readFile(join(MEDIA_DIR, name)).catch(() => {
    throw new Error(`image not found on this machine: ${ref}`)
  })
  const mime = mimeForExt(extOf(name))
  return `data:${mime};base64,${buf.toString('base64')}`
}

// ── Together HTTP ─────────────────────────────────────────────────────────────
function authHeaders(extra = {}) {
  return { Authorization: `Bearer ${API_KEY}`, ...extra }
}

async function postJSON(path, body, timeoutMs = HTTP_TIMEOUT_MS) {
  return doFetch(API_BASE + path, {
    method: 'POST',
    headers: authHeaders({ 'Content-Type': 'application/json', Accept: 'application/json' }),
    body: JSON.stringify(body),
  }, timeoutMs)
}

async function getJSON(path, timeoutMs = HTTP_TIMEOUT_MS) {
  return doFetch(API_BASE + path, { method: 'GET', headers: authHeaders({ Accept: 'application/json' }) }, timeoutMs)
}

async function doFetch(url, init, timeoutMs) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  let res
  try {
    res = await fetch(url, { ...init, signal: controller.signal })
  } catch (e) {
    clearTimeout(timer)
    const reason = e && e.name === 'AbortError' ? `timed out after ${timeoutMs}ms` : (e && e.message) || String(e)
    throw new Error(`${init.method} ${hostOf(url)} failed: ${reason}`)
  }
  clearTimeout(timer)
  const raw = await res.text().catch(() => '')
  let parsed = null
  try { parsed = raw ? JSON.parse(raw) : null } catch { /* non-JSON */ }
  if (!res.ok) {
    const msg = parsed?.error?.message || parsed?.error || parsed?.message || raw.slice(0, 300) || `HTTP ${res.status}`
    throw new Error(`Together ${res.status}: ${msg}`)
  }
  return parsed ?? {}
}

function hostOf(url) { try { return new URL(url).host } catch { return url } }

// ── tool implementations ──────────────────────────────────────────────────────
async function generateImage(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('generate_image', "a non-empty 'prompt' is required")
  const body = {
    model: IMAGE_MODEL,
    prompt,
    steps: 28,
    n: 1,
    response_format: 'base64',
    aspect_ratio: aspectRatio(args?.aspect_ratio),
  }
  if (Number.isInteger(args?.seed)) body.seed = args.seed
  const out = await postJSON('/v1/images/generations', body)
  return shapeImageOut('generate_image', out, prompt)
}

async function editImage(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('edit_image', "a non-empty 'prompt' is required")
  if (!args?.image) return errResult('edit_image', "an 'image' reference is required")
  const image_url = await resolveImageInput(args.image)
  const body = {
    model: IMAGE_MODEL,
    prompt,
    image_url,
    steps: 28,
    n: 1,
    response_format: 'base64',
    aspect_ratio: aspectRatio(args?.aspect_ratio),
  }
  if (Number.isInteger(args?.seed)) body.seed = args.seed
  const out = await postJSON('/v1/images/generations', body)
  return shapeImageOut('edit_image', out, prompt)
}

function aspectRatio(v) {
  const allowed = new Set(['1:1', '16:9', '9:16', '4:3', '3:2'])
  const s = String(v || '').trim()
  return allowed.has(s) ? s : '1:1'
}

async function shapeImageOut(tool, out, prompt) {
  const row = Array.isArray(out?.data) ? out.data[0] : null
  if (!row) return errResult(tool, 'no image returned by the model')
  let buf
  if (row.b64_json) {
    buf = Buffer.from(row.b64_json, 'base64')
  } else if (row.url) {
    buf = Buffer.from(await fetchBytes(row.url))
  } else {
    return errResult(tool, 'image response had neither b64_json nor url')
  }
  const { ext, mime } = sniffImage(buf)
  const w = await writeOutput(buf, ext, mime)
  return okResult({ ok: true, tool, kind: 'image', url: w.url, mime: w.mime, bytes: w.bytes, model: IMAGE_MODEL, prompt })
}

async function generateVideo(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('generate_video', "a non-empty 'prompt' is required")
  const body = { model: VIDEO_MODEL, prompt }
  if (args?.image) body.image_url = await resolveImageInput(args.image)
  if (Number.isInteger(args?.seconds) && args.seconds > 0) body.seconds = args.seconds
  const ar = String(args?.aspect_ratio || '').trim()
  if (ar) body.aspect_ratio = aspectRatio(ar)

  const job = await postJSON('/v2/videos', body)
  const id = job?.id
  if (!id) return errResult('generate_video', 'video job was not created (no id returned)', { raw: job })

  // Poll until the job settles or we hit the wait ceiling.
  const deadline = Date.now() + VIDEO_MAX_WAIT_MS
  let status = job
  while ((status?.status === 'queued' || status?.status === 'in_progress' || !status?.status)) {
    if (Date.now() > deadline) {
      return errResult('generate_video', `video still '${status?.status || 'pending'}' after ${Math.round(VIDEO_MAX_WAIT_MS / 1000)}s`, { job_id: id })
    }
    await sleep(VIDEO_POLL_MS)
    status = await getJSON(`/v2/videos/${encodeURIComponent(id)}`)
  }
  if (status.status !== 'completed') {
    return errResult('generate_video', status?.error?.message || `video generation ${status.status}`, { job_id: id })
  }
  const videoURL = status?.outputs?.video_url
  if (!videoURL) return errResult('generate_video', 'completed but no video_url in outputs', { job_id: id })
  // Together video URLs expire — download to the volume immediately.
  const buf = Buffer.from(await fetchBytes(videoURL))
  const w = await writeOutput(buf, 'mp4', 'video/mp4')
  return okResult({ ok: true, tool: 'generate_video', kind: 'video', url: w.url, mime: w.mime, bytes: w.bytes, model: VIDEO_MODEL, prompt, seconds: status?.seconds })
}

async function transcribeAudio(args) {
  const ref = String(args?.audio || '').trim()
  if (!ref) return errResult('transcribe_audio', "an 'audio' reference is required")
  const form = new FormData()
  form.append('model', AUDIO_MODEL)
  const lang = String(args?.language || '').trim()
  if (lang) form.append('language', lang)
  form.append('response_format', 'json')

  if (/^https?:\/\//i.test(ref)) {
    form.append('url', ref)
  } else {
    const name = localName(ref)
    if (!name) return errResult('transcribe_audio', `cannot resolve audio reference: ${ref}`)
    const buf = await readFile(join(MEDIA_DIR, name)).catch(() => null)
    if (!buf) return errResult('transcribe_audio', `audio not found on this machine: ${ref}`)
    form.append('file', new Blob([buf], { type: mimeForExt(extOf(name)) }), name)
  }

  const out = await doFetch(API_BASE + '/v1/audio/transcriptions', {
    method: 'POST',
    headers: authHeaders(), // let fetch set the multipart Content-Type + boundary
    body: form,
  }, HTTP_TIMEOUT_MS)
  const text = typeof out?.text === 'string' ? out.text : ''
  return okResult({ ok: true, tool: 'transcribe_audio', kind: 'transcript', text, language: lang || 'auto' })
}

async function fetchBytes(url) {
  const res = await doFetchRaw(url)
  if (!res.ok) throw new Error(`download ${hostOf(url)} failed: HTTP ${res.status}`)
  return res.arrayBuffer()
}

async function doFetchRaw(url) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), HTTP_TIMEOUT_MS)
  try {
    return await fetch(url, { signal: controller.signal })
  } finally {
    clearTimeout(timer)
  }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)) }

const impls = {
  generate_image: generateImage,
  edit_image: editImage,
  generate_video: generateVideo,
  transcribe_audio: transcribeAudio,
}

// ── JSON-RPC stdio server (daemon-facing) ─────────────────────────────────────
const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? PROTOCOL_VERSION,
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    if (!TOOL_SET.has(name)) return errResult(name, `unknown tool: ${name}`)
    if (!API_KEY) {
      return errResult(name, 'media bridge not configured', { hint: 'set TOGETHER_API_KEY on the machine to enable image/video/audio' })
    }
    try {
      return await impls[name](args)
    } catch (err) {
      return errResult(name, err?.message ?? String(err))
    }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(obj) { process.stdout.write(JSON.stringify(obj) + '\n') }
const rpcOk = (id, result) => ({ jsonrpc: '2.0', id, result })
const rpcErr = (id, code, message) => ({ jsonrpc: '2.0', id, error: { code, message } })

function startStdioServer() {
  const rl = createInterface({ input: process.stdin })
  rl.on('line', async (line) => {
    if (!line.trim()) return
    let req
    try {
      req = JSON.parse(line)
    } catch (err) {
      send(rpcErr(null, -32700, 'parse error: ' + err.message))
      return
    }
    const fn = handlers[req.method]
    if (!fn) {
      if (req.id !== undefined) send(rpcErr(req.id, -32601, `method not found: ${req.method}`))
      return
    }
    try {
      const result = await fn(req.params)
      if (req.id !== undefined && result !== null) send(rpcOk(req.id, result))
    } catch (err) {
      if (req.id !== undefined) send(rpcErr(req.id, -32000, err?.message ?? String(err)))
    }
  })
  process.stdin.on('end', () => process.exit(0))
  process.on('SIGINT', () => process.exit(0))
  process.on('SIGTERM', () => process.exit(0))
}

// `--selftest`: list the registry, then verify it against every agent manifest
// that ships a media server. executor/mcp Manager.verifyTools makes any
// bridge<->manifest tool-set drift a FATAL daemon boot; this guard turns the
// same drift into a non-zero exit at build/CI time. Offline (no network).
// MATRIX_MEDIA_AGENTS_DIR overrides the manifest dir (used by tests).
function runSelftest() {
  console.log(`media: ${tools.length} tools (image=${IMAGE_MODEL}, video=${VIDEO_MODEL}, audio=${AUDIO_MODEL}, key=${API_KEY ? 'set' : 'UNSET'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.MATRIX_MEDIA_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`media SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }

  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`media FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'media')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`media FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot: "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot: "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`media: ${file} matches (${declared.size} tools)`)
    }
  }

  if (checked === 0) {
    console.error(`media SELFTEST FAILED: no manifest under ${agentsDir} declares a media server`)
    process.exit(1)
  }
  if (drift) {
    console.error('media SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }
  console.log(`media OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

if (process.argv.includes('--selftest')) {
  runSelftest()
} else {
  startStdioServer()
}
