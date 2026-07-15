import { timingSafeEqual } from 'node:crypto'
import { posix } from 'node:path'

const forwardedHeaders = new Set([
  'accept', 'accept-encoding', 'accept-language', 'authorization', 'cache-control',
  'content-type', 'cookie', 'if-match', 'if-modified-since', 'if-none-match',
  'origin', 'pragma', 'range', 'referer', 'user-agent', 'x-requested-with',
])

const responseHopHeaders = new Set([
  'connection', 'content-length', 'keep-alive', 'proxy-authenticate',
  'proxy-authorization', 'te', 'trailer', 'transfer-encoding', 'upgrade',
])

export function authorized(header, token) {
  const got = String(header || '').replace(/^Bearer\s+/i, '')
  const a = Buffer.from(got)
  const b = Buffer.from(token)
  return a.length === b.length && timingSafeEqual(a, b)
}

export function safeUser(value) {
  const user = String(value || '').trim()
  if (!user || user.length > 200 || /[\x00-\x20\x7f]/.test(user)) throw new Error('X-Matrix-User is required')
  return user
}

export function safePort(value) {
  const port = Number(value)
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('port must be an integer from 1 to 65535')
  return port
}

export function safeSandboxPath(value) {
  const raw = String(value || '').replaceAll('\\', '/')
  if (raw.split('/').includes('..')) throw new Error(`invalid file path ${JSON.stringify(value)}`)
  const clean = posix.normalize('/' + raw).slice(1)
  if (!raw || raw.startsWith('/') || clean === '..' || clean.startsWith('../')) throw new Error(`invalid file path ${JSON.stringify(value)}`)
  return `/workspace/${clean}`
}

export function decodeFiles(files, maxFiles, maxBytes) {
  if (!Array.isArray(files) || files.length === 0) throw new Error('files must be a non-empty array')
  if (files.length > maxFiles) throw new Error(`files exceeds limit ${maxFiles}`)
  let bytes = 0
  return files.map((file) => {
    const path = safeSandboxPath(file?.path)
    const content = file?.encoding === 'base64'
      ? Buffer.from(String(file.content || ''), 'base64')
      : Buffer.from(String(file?.content ?? ''), 'utf8')
    bytes += content.length
    if (bytes > maxBytes) throw new Error(`file payload exceeds ${maxBytes} bytes`)
    return { path, content, mode: file?.mode === 0o755 ? 0o755 : 0o644 }
  })
}

export function previewSlugFromHost(host, domain) {
  const hostname = String(host || '').toLowerCase().split(':')[0].replace(/\.$/, '')
  const suffix = `.${domain}`
  if (!hostname.endsWith(suffix)) return ''
  const slug = hostname.slice(0, -suffix.length)
  return /^[a-z0-9]{32}$/.test(slug) ? slug : ''
}

export function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\\''")}'`
}

export function curlHeaders(headers) {
  const args = []
  for (const [name, value] of Object.entries(headers || {})) {
    const lower = name.toLowerCase()
    if (!forwardedHeaders.has(lower)) continue
    const values = Array.isArray(value) ? value : [value]
    for (const item of values) args.push('-H', `${name}: ${item}`)
  }
  return args
}

export function parseCurlHeaders(raw) {
  const blocks = String(raw).replaceAll('\r\n', '\n').trim().split(/\n\n+/)
  const block = blocks.at(-1) || ''
  const lines = block.split('\n')
  const match = /^HTTP\/\S+\s+(\d{3})/.exec(lines.shift() || '')
  if (!match) throw new Error('sandbox app returned an invalid response')
  const headers = {}
  for (const line of lines) {
    const i = line.indexOf(':')
    if (i < 1) continue
    const name = line.slice(0, i).trim().toLowerCase()
    if (responseHopHeaders.has(name)) continue
    const value = line.slice(i + 1).trim()
    if (name === 'set-cookie') (headers[name] ||= []).push(value)
    else headers[name] = value
  }
  return { status: Number(match[1]), headers }
}

export async function readBody(req, limit) {
  const chunks = []
  let size = 0
  for await (const chunk of req) {
    size += chunk.length
    if (size > limit) throw Object.assign(new Error('request body too large'), { statusCode: 413 })
    chunks.push(chunk)
  }
  return Buffer.concat(chunks)
}
