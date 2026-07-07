// media — MCP stdio bridge giving Matrix agents (Neo + the MCL daemon) media
// I/O backed by the Novita AI media APIs: image generation / editing /
// inpainting, the specialized image utilities (remove/replace background,
// remove text, cleanup, merge faces), text-to-video / image-to-video, and
// text-to-speech.
//
// Why a local bridge (not a remote MCP)? Novita exposes plain REST media
// endpoints, not an MCP server. This bridge speaks the daemon's stdio JSON-RPC
// (executor/mcp/stdio) on one side and calls Novita on the other, shaping each
// result into a CallToolResult. It mirrors tools/paxeer/paxeer-net.mjs.
//
// Two Novita call shapes are handled:
//   - async  POST /v3/async/<op> -> {task_id}; poll GET
//            /v3/async/task-result?task_id=<id> until task.status SUCCEED/FAIL.
//            Used by txt2img, img2img, inpainting, txt2video, img2video,
//            txt2speech.
//   - sync   POST /v3/<op> -> result inline. Used by remove-background,
//            replace-background, remove-text, cleanup, merge-face.
//
// Storage: generated/edited outputs are written to MATRIX_MEDIA_DIR — the
// agent's OWN machine volume (e.g. /data/media), the same volume that holds
// cortex + executor.key — and the tool returns a relative {url} under
// MATRIX_MEDIA_BASE (default /media) that the Neo server streams back to the
// browser. Input refs (attachments / prior outputs) are resolved off the SAME
// volume and inlined to Novita as base64, so each tool's output {url} can be
// fed straight into the next tool's image arg — Neo chains them (e.g. remove
// background, then feed the result into remove text, then present).
//
// Auth: NOVITA_API_KEY (Bearer). Without it the media tools return a
// structured error rather than bricking daemon boot (spawn stays non-fatal).
//
// Models (env-overridable; only the async generation ops need a checkpoint —
// the sync image utilities are model-less):
//   MATRIX_MEDIA_IMAGE_MODEL      default sd_xl_base_1.0.safetensors  (txt2img/img2img)
//   MATRIX_MEDIA_INPAINT_MODEL    default realisticVisionV51_v51VAE-inpainting_94324.safetensors
//   MATRIX_MEDIA_VIDEO_MODEL      default darkSushiMixMix_225D_64380.safetensors (txt2video)
//   MATRIX_MEDIA_IMG2VIDEO_MODEL  default SVD-XT  (img2video)
//   MATRIX_MEDIA_TTS_VOICE        default Emily   MATRIX_MEDIA_TTS_LANGUAGE default en-US
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
const SERVER_VERSION = '0.2.0'
const PROTOCOL_VERSION = '2024-11-05'

const API_BASE = (process.env.MATRIX_MEDIA_API_BASE || 'https://api.novita.ai').replace(/\/+$/, '')
const API_KEY = (process.env.NOVITA_API_KEY || '').trim()

const IMAGE_MODEL = (process.env.MATRIX_MEDIA_IMAGE_MODEL || 'sd_xl_base_1.0.safetensors').trim()
const INPAINT_MODEL = (process.env.MATRIX_MEDIA_INPAINT_MODEL || 'realisticVisionV51_v51VAE-inpainting_94324.safetensors').trim()
const VIDEO_MODEL = (process.env.MATRIX_MEDIA_VIDEO_MODEL || 'darkSushiMixMix_225D_64380.safetensors').trim()
const IMG2VIDEO_MODEL = (process.env.MATRIX_MEDIA_IMG2VIDEO_MODEL || 'SVD-XT').trim()
const TTS_VOICE = (process.env.MATRIX_MEDIA_TTS_VOICE || 'Emily').trim()
const TTS_LANGUAGE = (process.env.MATRIX_MEDIA_TTS_LANGUAGE || 'en-US').trim()

const MEDIA_DIR = (process.env.MATRIX_MEDIA_DIR || join(tmpdir(), 'matrix-media')).replace(/\/+$/, '')
const MEDIA_BASE = '/' + (process.env.MATRIX_MEDIA_BASE || '/data/media').replace(/^\/+|\/+$/g, '')

const HTTP_TIMEOUT_MS = clampInt(process.env.MATRIX_MEDIA_TIMEOUT_MS, 120000, 5000, 300000)
const TASK_POLL_MS = clampInt(process.env.MATRIX_MEDIA_TASK_POLL_MS, 3000, 1000, 60000)
const TASK_MAX_WAIT_MS = clampInt(process.env.MATRIX_MEDIA_TASK_MAX_WAIT_MS, 540000, 30000, 1800000)

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

function clampFloat(v, def, min, max) {
  const n = Number.parseFloat(v ?? '')
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

// Sniff a small set of image magic numbers so the written extension and served
// MIME match the actual bytes Novita returns.
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

// resolveBase64 turns any image/audio ref into a RAW base64 string (no data:
// prefix) for Novita's *_base64 / image_file / mask fields. data: URIs are
// unwrapped, public URLs are downloaded, and /media refs are read off the
// shared volume — so private attachments and chained outputs never need to be
// publicly hosted.
async function resolveBase64(ref) {
  const s = String(ref || '').trim()
  if (!s) throw new Error('empty media reference')
  if (/^data:/i.test(s)) {
    const comma = s.indexOf(',')
    return comma >= 0 ? s.slice(comma + 1) : ''
  }
  if (/^https?:\/\//i.test(s)) {
    const buf = Buffer.from(await fetchBytes(s))
    return buf.toString('base64')
  }
  const name = localName(s)
  if (!name) throw new Error(`cannot resolve media reference: ${ref}`)
  const buf = await readFile(join(MEDIA_DIR, name)).catch(() => {
    throw new Error(`media not found on this machine: ${ref}`)
  })
  return buf.toString('base64')
}

// ── Novita HTTP ───────────────────────────────────────────────────────────────
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
    const msg = parsed?.message || parsed?.error?.message || parsed?.error || parsed?.reason || raw.slice(0, 300) || `HTTP ${res.status}`
    throw new Error(`Novita ${res.status}: ${msg}`)
  }
  return parsed ?? {}
}

function hostOf(url) { try { return new URL(url).host } catch { return url } }

// runAsyncTask POSTs to an async op, then polls task-result until the task
// settles. Returns the settled result object ({task, images?, videos?,
// audios?}). Novita status strings are TASK_STATUS_{QUEUED,PROCESSING,SUCCEED,
// FAILED}; we match on the SUCCEED/FAIL substrings to be tolerant of the bare
// vs prefixed forms the API has used.
async function runAsyncTask(path, body) {
  const job = await postJSON(path, body)
  const taskId = job?.task_id || job?.id
  if (!taskId) throw new Error(`no task_id returned (raw: ${JSON.stringify(job).slice(0, 200)})`)
  const deadline = Date.now() + TASK_MAX_WAIT_MS
  for (;;) {
    const res = await getJSON(`/v3/async/task-result?task_id=${encodeURIComponent(taskId)}`)
    const status = String(res?.task?.status || '').toUpperCase()
    if (status.includes('SUCCEED')) return res
    if (status.includes('FAIL')) throw new Error(res?.task?.reason || `task ${taskId} failed`)
    if (Date.now() > deadline) {
      throw new Error(`task ${taskId} still '${status || 'pending'}' after ${Math.round(TASK_MAX_WAIT_MS / 1000)}s`)
    }
    await sleep(TASK_POLL_MS)
  }
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

// ── output extraction + persistence ───────────────────────────────────────────
function firstImageURL(res) {
  const imgs = res?.images
  if (Array.isArray(imgs) && imgs.length) return imgs[0]?.image_url || imgs[0]?.url || null
  return null
}
function firstVideoURL(res) {
  const vids = res?.videos
  if (Array.isArray(vids) && vids.length) return vids[0]?.video_url || vids[0]?.url || null
  return null
}
function firstAudioURL(res) {
  const auds = res?.audios
  if (Array.isArray(auds) && auds.length) return auds[0]?.audio_url || auds[0]?.url || null
  return null
}

// storeImageString persists a Novita image result that may be either a hosted
// (expiring) URL or a base64 / data-URI string, sniffing the real bytes.
async function storeImageString(s) {
  let buf
  if (/^https?:\/\//i.test(s)) {
    buf = Buffer.from(await fetchBytes(s))
  } else {
    const raw = s.includes(',') ? s.slice(s.indexOf(',') + 1) : s
    buf = Buffer.from(raw, 'base64')
  }
  const { ext, mime } = sniffImage(buf)
  return writeOutput(buf, ext, mime)
}

// shapeSyncImageOut handles the inline response from the sync image utilities,
// whose field name for the processed image varies by op (image_file / image /
// image_url / images[]).
async function shapeSyncImageOut(tool, out, extra = {}) {
  const cand = out?.image_file ?? out?.image ?? out?.image_url
    ?? (Array.isArray(out?.images) ? (out.images[0]?.image_url || out.images[0]?.image_file || out.images[0]) : null)
  if (!cand || typeof cand !== 'string') {
    return errResult(tool, 'no image in response', { keys: Object.keys(out || {}) })
  }
  const w = await storeImageString(cand)
  return okResult({ ok: true, tool, kind: 'image', url: w.url, mime: w.mime, bytes: w.bytes, ...extra })
}

// aspect-ratio → SDXL-friendly pixel dimensions for txt2img/img2img/inpainting.
function dimsFor(v) {
  switch (String(v || '').trim()) {
    case '16:9': return { width: 1344, height: 768 }
    case '9:16': return { width: 768, height: 1344 }
    case '4:3': return { width: 1152, height: 896 }
    case '3:2': return { width: 1216, height: 832 }
    case '1:1':
    default: return { width: 1024, height: 1024 }
  }
}

// aspect-ratio → video dimensions (kept modest for the SD1.5-class default).
function videoDimsFor(v) {
  switch (String(v || '').trim()) {
    case '9:16': return { width: 480, height: 640 }
    case '1:1': return { width: 512, height: 512 }
    case '16:9':
    default: return { width: 640, height: 480 }
  }
}

const TTS_VOICES = new Set(['Emily', 'James', 'Olivia', 'Michael', 'Sarah', 'John'])
const TTS_LANGS = new Set(['en-US', 'zh-CN', 'ja-JP'])
function pickVoice(v) { const s = String(v || '').trim(); return TTS_VOICES.has(s) ? s : TTS_VOICE }
function pickLang(v) { const s = String(v || '').trim(); return TTS_LANGS.has(s) ? s : TTS_LANGUAGE }

function seedOf(args) { return Number.isInteger(args?.seed) ? args.seed : -1 }

// ── tool implementations ──────────────────────────────────────────────────────
async function generateImage(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('generate_image', "a non-empty 'prompt' is required")
  const { width, height } = dimsFor(args?.aspect_ratio)
  const request = {
    model_name: IMAGE_MODEL,
    prompt,
    negative_prompt: String(args?.negative_prompt || '').trim(),
    width, height,
    sampler_name: 'DPM++ 2M Karras',
    guidance_scale: 7.5,
    steps: 25,
    image_num: 1,
    clip_skip: 1,
    seed: seedOf(args),
  }
  const res = await runAsyncTask('/v3/async/txt2img', { request, extra: { response_image_type: 'png' } })
  const url = firstImageURL(res)
  if (!url) return errResult('generate_image', 'no image in task result', { keys: Object.keys(res || {}) })
  const w = await storeImageString(url)
  return okResult({ ok: true, tool: 'generate_image', kind: 'image', url: w.url, mime: w.mime, bytes: w.bytes, model: IMAGE_MODEL, prompt })
}

async function editImage(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('edit_image', "a non-empty 'prompt' is required")
  if (!args?.image) return errResult('edit_image', "an 'image' reference is required")
  const image_base64 = await resolveBase64(args.image)
  const { width, height } = dimsFor(args?.aspect_ratio)
  const request = {
    model_name: IMAGE_MODEL,
    image_base64,
    prompt,
    negative_prompt: String(args?.negative_prompt || '').trim(),
    width, height,
    sampler_name: 'DPM++ 2M Karras',
    guidance_scale: 7.5,
    steps: 25,
    image_num: 1,
    clip_skip: 1,
    strength: clampFloat(args?.strength, 0.7, 0, 1),
    seed: seedOf(args),
  }
  const res = await runAsyncTask('/v3/async/img2img', { request, extra: { response_image_type: 'png' } })
  const url = firstImageURL(res)
  if (!url) return errResult('edit_image', 'no image in task result', { keys: Object.keys(res || {}) })
  const w = await storeImageString(url)
  return okResult({ ok: true, tool: 'edit_image', kind: 'image', url: w.url, mime: w.mime, bytes: w.bytes, model: IMAGE_MODEL, prompt })
}

async function inpaintImage(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('inpaint_image', "a non-empty 'prompt' is required")
  if (!args?.image) return errResult('inpaint_image', "an 'image' reference is required")
  if (!args?.mask) return errResult('inpaint_image', "a 'mask' reference is required (white = area to repaint)")
  const image_base64 = await resolveBase64(args.image)
  const mask_image_base64 = await resolveBase64(args.mask)
  const body = {
    model_name: INPAINT_MODEL,
    image_base64,
    mask_image_base64,
    prompt,
    negative_prompt: String(args?.negative_prompt || '').trim(),
    image_num: 1,
    mask_blur: 0,
    sampler_name: 'DPM++ 2M Karras',
    clip_skip: 0,
    guidance_scale: 7,
    steps: 20,
    strength: clampFloat(args?.strength, 1, 0, 1),
    seed: seedOf(args),
    inpainting_full_res: false,
    inpainting_full_res_padding: 0,
    inpainting_mask_invert: false,
    initial_noise_multiplier: 0,
  }
  const res = await runAsyncTask('/v3/async/inpainting', body)
  const url = firstImageURL(res)
  if (!url) return errResult('inpaint_image', 'no image in task result', { keys: Object.keys(res || {}) })
  const w = await storeImageString(url)
  return okResult({ ok: true, tool: 'inpaint_image', kind: 'image', url: w.url, mime: w.mime, bytes: w.bytes, model: INPAINT_MODEL, prompt })
}

async function removeBackground(args) {
  if (!args?.image) return errResult('remove_background', "an 'image' reference is required")
  const image_file = await resolveBase64(args.image)
  const out = await postJSON('/v3/remove-background', { image_file })
  return shapeSyncImageOut('remove_background', out)
}

async function replaceBackground(args) {
  if (!args?.image) return errResult('replace_background', "an 'image' reference is required")
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('replace_background', "a non-empty 'prompt' (the new background to place) is required")
  const image_file = await resolveBase64(args.image)
  const out = await postJSON('/v3/replace-background', { image_file, prompt })
  return shapeSyncImageOut('replace_background', out, { prompt })
}

async function removeText(args) {
  if (!args?.image) return errResult('remove_text', "an 'image' reference is required")
  const image_file = await resolveBase64(args.image)
  const out = await postJSON('/v3/remove-text', { image_file })
  return shapeSyncImageOut('remove_text', out)
}

async function cleanupImage(args) {
  if (!args?.image) return errResult('cleanup_image', "an 'image' reference is required")
  if (!args?.mask) return errResult('cleanup_image', "a 'mask' reference is required (white = object to erase)")
  const image_file = await resolveBase64(args.image)
  const mask_file = await resolveBase64(args.mask)
  const out = await postJSON('/v3/cleanup', { image_file, mask_file })
  return shapeSyncImageOut('cleanup_image', out)
}

async function mergeFaces(args) {
  if (!args?.image) return errResult('merge_faces', "an 'image' reference (the base photo) is required")
  if (!args?.face) return errResult('merge_faces', "a 'face' reference (the face to swap in) is required")
  const image_file = await resolveBase64(args.image)
  const face_image_file = await resolveBase64(args.face)
  const out = await postJSON('/v3/merge-face', { image_file, face_image_file })
  return shapeSyncImageOut('merge_faces', out)
}

async function generateVideo(args) {
  const prompt = String(args?.prompt || '').trim()
  if (!prompt) return errResult('generate_video', "a non-empty 'prompt' is required")
  const { width, height } = videoDimsFor(args?.aspect_ratio)
  const frames = clampInt(args?.frames, 64, 16, 128)
  const body = {
    model_name: VIDEO_MODEL,
    width, height,
    guidance_scale: 7.5,
    steps: 20,
    seed: seedOf(args),
    prompts: [{ prompt, frames }],
    negative_prompt: String(args?.negative_prompt || '').trim(),
  }
  const res = await runAsyncTask('/v3/async/txt2video', body)
  const url = firstVideoURL(res)
  if (!url) return errResult('generate_video', 'no video in task result', { keys: Object.keys(res || {}) })
  const buf = Buffer.from(await fetchBytes(url))
  const w = await writeOutput(buf, 'mp4', 'video/mp4')
  return okResult({ ok: true, tool: 'generate_video', kind: 'video', url: w.url, mime: w.mime, bytes: w.bytes, model: VIDEO_MODEL, prompt })
}

async function animateImage(args) {
  if (!args?.image) return errResult('animate_image', "an 'image' reference is required")
  const image_file = await resolveBase64(args.image)
  const body = {
    model_name: IMG2VIDEO_MODEL,
    image_file,
    frames_num: clampInt(args?.frames, 25, 14, 50),
    frames_per_second: clampInt(args?.fps, 6, 1, 30),
    seed: seedOf(args),
    image_file_resize_mode: 'ORIGINAL_RESOLUTION',
    steps: 20,
  }
  const res = await runAsyncTask('/v3/async/img2video', body)
  const url = firstVideoURL(res)
  if (!url) return errResult('animate_image', 'no video in task result', { keys: Object.keys(res || {}) })
  const buf = Buffer.from(await fetchBytes(url))
  const w = await writeOutput(buf, 'mp4', 'video/mp4')
  return okResult({ ok: true, tool: 'animate_image', kind: 'video', url: w.url, mime: w.mime, bytes: w.bytes, model: IMG2VIDEO_MODEL })
}

async function generateSpeech(args) {
  const text = String(args?.text || '').trim()
  if (!text) return errResult('generate_speech', "a non-empty 'text' is required")
  if (text.length > 512) return errResult('generate_speech', 'text exceeds the 512-character Novita TTS limit')
  const voice = pickVoice(args?.voice)
  const language = pickLang(args?.language)
  const request = {
    voice_id: voice,
    language,
    texts: [text],
    volume: clampFloat(args?.volume, 1.0, 1.0, 2.0),
    speed: clampFloat(args?.speed, 1.0, 0.8, 3.0),
  }
  const res = await runAsyncTask('/v3/async/txt2speech', { request, extra: { response_audio_type: 'mp3' } })
  const url = firstAudioURL(res)
  if (!url) return errResult('generate_speech', 'no audio in task result', { keys: Object.keys(res || {}) })
  const buf = Buffer.from(await fetchBytes(url))
  const w = await writeOutput(buf, 'mp3', 'audio/mpeg')
  return okResult({ ok: true, tool: 'generate_speech', kind: 'audio', url: w.url, mime: w.mime, bytes: w.bytes, voice, language })
}

const impls = {
  generate_image: generateImage,
  edit_image: editImage,
  inpaint_image: inpaintImage,
  remove_background: removeBackground,
  replace_background: replaceBackground,
  remove_text: removeText,
  cleanup_image: cleanupImage,
  merge_faces: mergeFaces,
  generate_video: generateVideo,
  animate_image: animateImage,
  generate_speech: generateSpeech,
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
      return errResult(name, 'media bridge not configured', { hint: 'set NOVITA_API_KEY on the machine to enable image/video/audio' })
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
  console.log(`media: ${tools.length} tools (provider=novita, image=${IMAGE_MODEL}, inpaint=${INPAINT_MODEL}, video=${VIDEO_MODEL}, img2video=${IMG2VIDEO_MODEL}, tts=${TTS_VOICE}/${TTS_LANGUAGE}, key=${API_KEY ? 'set' : 'UNSET'})`)
  for (const t of tools) console.log(`  - ${t.name}`)

  // Every advertised tool must have an implementation, and vice versa.
  const implNames = new Set(Object.keys(impls))
  const noImpl = TOOL_NAMES.filter((n) => !implNames.has(n))
  const noTool = [...implNames].filter((n) => !TOOL_SET.has(n))
  if (noImpl.length || noTool.length) {
    if (noImpl.length) console.error(`media FAIL: tools advertised with no impl: ${noImpl.join(', ')}`)
    if (noTool.length) console.error(`media FAIL: impls with no advertised tool: ${noTool.join(', ')}`)
    process.exit(1)
  }

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