import { randomBytes } from 'node:crypto'
import { Sandbox } from 'railway'
import { curlHeaders, decodeFiles, parseCurlHeaders, safePort, shellQuote } from './protocol.mjs'

const metadataPath = '/.matrix-preview.json'

export class SandboxManager {
  constructor(config) {
    this.config = config
    this.records = new Map()
    this.timer = null
  }

  options(extra = {}) {
    return {
      token: this.config.railwayToken,
      authType: this.config.railwayAuthType,
      environmentId: this.config.environmentId,
      ...extra,
    }
  }

  async recover() {
    const listed = await Sandbox.list(this.options({ first: 100 }))
    for (const info of listed) {
      if (info.status !== 'RUNNING') continue
      try {
        const sandbox = await Sandbox.connect(info.id, this.options())
        const record = JSON.parse(await sandbox.files.read(metadataPath))
        if (record?.kind !== 'matrix-preview-v1' || !record.slug || !record.userId) continue
        record.sandboxId = info.id
        this.records.set(record.slug, record)
      } catch {}
    }
  }

  startReaper() {
    this.timer = setInterval(() => void this.reap(), 60_000)
    this.timer.unref()
  }

  async close() {
    if (this.timer) clearInterval(this.timer)
  }

  list(userId) {
    return [...this.records.values()].filter((r) => r.userId === userId).map((r) => this.publicRecord(r))
  }

  getOwned(userId, slug) {
    const record = this.records.get(slug)
    if (!record || record.userId !== userId) return null
    return record
  }

  publicRecord(record) {
    return {
      id: record.slug,
      status: record.status,
      preview_url: `${this.config.publicScheme}://${record.slug}.${this.config.previewDomain}/`,
      port: record.port,
      created_at: record.createdAt,
      expires_at: record.expiresAt,
    }
  }

  async create(userId, input) {
    if (this.list(userId).length >= this.config.maxPerUser) {
      throw Object.assign(new Error(`sandbox limit reached (${this.config.maxPerUser})`), { statusCode: 429 })
    }
    const port = safePort(input.port)
    const ttl = Math.min(Number(input.ttl_seconds) || this.config.defaultTTLSeconds, this.config.maxTTLSeconds)
    if (!Number.isFinite(ttl) || ttl < 60) throw new Error('ttl_seconds must be at least 60')
    const files = decodeFiles(input.files, this.config.maxFiles, this.config.maxUploadBytes)
    const startCommand = String(input.start_command || '').trim()
    if (!startCommand) throw new Error('start_command is required')
    const env = Object.fromEntries(Object.entries(input.env || {}).map(([k, v]) => [String(k), String(v)]))
    env.PORT = String(port)
    env.HOST = '0.0.0.0'

    const requestedPackages = Array.isArray(input.packages) ? input.packages.map(String) : []
    if (requestedPackages.length > 20 || requestedPackages.some((name) => !/^[a-z0-9][a-z0-9+.-]{0,63}$/.test(name))) {
      throw new Error('packages must contain at most 20 valid Debian package names')
    }
    const template = Sandbox.template().withPackages(...new Set(['curl', ...requestedPackages])).workdir('/workspace')

    const sandbox = await Sandbox.create(template, this.options({
      idleTimeoutMinutes: Math.ceil(ttl / 60),
      networkIsolation: 'ISOLATED',
      env,
    }))
    let keep = false
    try {
      await sandbox.files.mkdir('/workspace')
      for (const file of files) await sandbox.files.write(file.path, file.content, { mode: file.mode })
      const install = String(input.install_command || '').trim()
      if (install) {
        const result = await sandbox.exec(install, { cwd: '/workspace', timeoutSec: 900 })
        if (result.exitCode !== 0) throw new Error(`install command failed: ${result.stderr || result.stdout}`.slice(0, 2000))
      }
      const handle = sandbox.exec(startCommand, { cwd: '/workspace' })
      const sessionName = await handle.sessionName
      await handle.detach()
      await this.waitHealthy(sandbox, port)
      const now = Date.now()
      const record = {
        kind: 'matrix-preview-v1',
        slug: randomBytes(16).toString('hex'),
        sandboxId: sandbox.id,
        userId,
        port,
        sessionName,
        status: 'ready',
        createdAt: new Date(now).toISOString(),
        touchedAt: new Date(now).toISOString(),
        expiresAt: new Date(now + ttl * 1000).toISOString(),
      }
      await sandbox.files.write(metadataPath, JSON.stringify(record), { mode: 0o600 })
      this.records.set(record.slug, record)
      keep = true
      return this.publicRecord(record)
    } finally {
      if (!keep) await sandbox.destroy().catch(() => {})
    }
  }

  async waitHealthy(sandbox, port) {
    for (let i = 0; i < 40; i++) {
      const result = await sandbox.exec(`curl -sS -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:${port}/`, { timeoutSec: 5 })
      if (result.exitCode === 0 && /^[234]\d\d$/.test(result.stdout.trim())) return
      await new Promise((resolve) => setTimeout(resolve, 3000))
    }
    throw new Error(`app did not become reachable on port ${port}`)
  }

  async exec(userId, slug, command, timeoutSec = 120) {
    const record = this.getOwned(userId, slug)
    if (!record) return null
    const sandbox = await Sandbox.connect(record.sandboxId, this.options())
    const result = await sandbox.exec(String(command), { cwd: '/workspace', timeoutSec: Math.min(Number(timeoutSec) || 120, 900) })
    this.touch(record)
    return result
  }

  async writeFiles(userId, slug, files) {
    const record = this.getOwned(userId, slug)
    if (!record) return null
    const decoded = decodeFiles(files, this.config.maxFiles, this.config.maxUploadBytes)
    const sandbox = await Sandbox.connect(record.sandboxId, this.options())
    for (const file of decoded) await sandbox.files.write(file.path, file.content, { mode: file.mode })
    this.touch(record)
    return this.publicRecord(record)
  }

  async destroy(userId, slug) {
    const record = this.getOwned(userId, slug)
    if (!record) return false
    this.records.delete(slug)
    const sandbox = await Sandbox.connect(record.sandboxId, this.options())
    await sandbox.destroy()
    return true
  }

  touch(record) {
    record.touchedAt = new Date().toISOString()
  }

  async reap() {
    const now = Date.now()
    for (const record of [...this.records.values()]) {
      if (Date.parse(record.expiresAt) > now) continue
      this.records.delete(record.slug)
      try {
        const sandbox = await Sandbox.connect(record.sandboxId, this.options())
        await sandbox.destroy()
      } catch {}
    }
  }

  async proxy(record, request) {
    const sandbox = await Sandbox.connect(record.sandboxId, this.options())
    const requestId = randomBytes(12).toString('hex')
    const base = `/tmp/matrix-preview-${requestId}`
    if (request.body.length) await sandbox.files.write(`${base}.in`, request.body, { mode: 0o600 })
    const args = ['curl', '-sS', '--max-time', '90', '-D', `${base}.headers`, '-o', `${base}.out`, '-X', request.method]
    args.push(...curlHeaders(request.headers))
    args.push('-H', `Host: ${request.host}`)
    args.push('-H', `X-Forwarded-Host: ${request.host}`)
    args.push('-H', `X-Forwarded-Proto: ${this.config.publicScheme}`)
    if (request.body.length) args.push('--data-binary', `@${base}.in`)
    args.push(`http://127.0.0.1:${record.port}${request.path}`)
    const command = `${args.map(shellQuote).join(' ')}; rc=$?; echo $rc`
    try {
      const result = await sandbox.exec(command, { timeoutSec: 100 })
      if (result.exitCode !== 0 || result.stdout.trim().split('\n').at(-1) !== '0') throw new Error('sandbox app is unavailable')
      const parsed = parseCurlHeaders(await sandbox.files.read(`${base}.headers`))
      const body = await sandbox.files.read(`${base}.out`, { format: 'bytes' })
      this.touch(record)
      return { ...parsed, body: Buffer.from(body) }
    } finally {
      await sandbox.exec(`rm -f ${shellQuote(base + '.in')} ${shellQuote(base + '.headers')} ${shellQuote(base + '.out')}`, { timeoutSec: 10 }).catch(() => {})
    }
  }
}
