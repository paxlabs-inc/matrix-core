import http from 'node:http'
import { pathToFileURL } from 'node:url'
import { loadConfig } from './config.mjs'
import { SandboxManager } from './manager.mjs'
import { authorized, previewSlugFromHost, readBody, safeUser } from './protocol.mjs'

function json(res, status, value) {
  const body = Buffer.from(JSON.stringify(value))
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': body.length })
  res.end(body)
}

function error(res, err) {
  const status = err.statusCode || (/required|must|invalid|files|path|port|ttl/i.test(err.message) ? 400 : 500)
  json(res, status, { error: status >= 500 ? 'sandbox operation failed' : err.message })
  if (status >= 500) console.error(err)
}

async function inputJSON(req, limit) {
  const body = await readBody(req, limit)
  try { return body.length ? JSON.parse(body) : {} } catch { throw new Error('invalid JSON body') }
}

export function createServer(config, manager) {
  return http.createServer(async (req, res) => {
    try {
      const slug = previewSlugFromHost(req.headers.host, config.previewDomain)
      if (slug) {
        const record = manager.records.get(slug)
        if (!record) return json(res, 404, { error: 'preview not found' })
        if (String(req.headers.upgrade || '').toLowerCase() === 'websocket') return json(res, 426, { error: 'preview websocket upgrades are not supported' })
        const body = await readBody(req, config.maxProxyBodyBytes)
        const reply = await manager.proxy(record, {
          method: req.method,
          path: req.url || '/',
          host: req.headers.host,
          headers: req.headers,
          body,
        })
        for (const [name, value] of Object.entries(reply.headers)) res.setHeader(name, value)
        res.setHeader('content-length', reply.body.length)
        res.writeHead(reply.status)
        return res.end(req.method === 'HEAD' ? undefined : reply.body)
      }

      if (req.url === '/healthz') return json(res, 200, { status: 'ok' })
      if (!authorized(req.headers.authorization, config.token)) return json(res, 401, { error: 'unauthorized' })
      const userId = safeUser(req.headers['x-matrix-user'])
      const url = new URL(req.url || '/', 'http://sandboxd.internal')
      const match = /^\/v1\/sandboxes\/([a-z0-9]{32})(?:\/(exec|files))?$/.exec(url.pathname)

      if (url.pathname === '/v1/sandboxes' && req.method === 'POST') {
        return json(res, 201, await manager.create(userId, await inputJSON(req, config.maxUploadBytes + 1_000_000)))
      }
      if (url.pathname === '/v1/sandboxes' && req.method === 'GET') return json(res, 200, { sandboxes: manager.list(userId) })
      if (!match) return json(res, 404, { error: 'not found' })
      const [, id, action] = match
      if (!action && req.method === 'GET') {
        const record = manager.getOwned(userId, id)
        return record ? json(res, 200, manager.publicRecord(record)) : json(res, 404, { error: 'sandbox not found' })
      }
      if (!action && req.method === 'DELETE') {
        if (!(await manager.destroy(userId, id))) return json(res, 404, { error: 'sandbox not found' })
        res.writeHead(204)
        return res.end()
      }
      if (action === 'exec' && req.method === 'POST') {
        const input = await inputJSON(req, 1_000_000)
        if (!String(input.command || '').trim()) throw new Error('command is required')
        const result = await manager.exec(userId, id, input.command, input.timeout_seconds)
        return result ? json(res, 200, result) : json(res, 404, { error: 'sandbox not found' })
      }
      if (action === 'files' && req.method === 'PUT') {
        const result = await manager.writeFiles(userId, id, (await inputJSON(req, config.maxUploadBytes + 1_000_000)).files)
        return result ? json(res, 200, result) : json(res, 404, { error: 'sandbox not found' })
      }
      return json(res, 405, { error: 'method not allowed' })
    } catch (err) {
      error(res, err)
    }
  })
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const config = loadConfig()
  const manager = new SandboxManager(config)
  await manager.recover()
  manager.startReaper()
  const server = createServer(config, manager)
  server.listen(config.port, config.addr, () => console.error(`sandboxd listening on ${config.addr}:${config.port}`))
  const stop = async () => {
    server.close()
    await manager.close()
  }
  process.once('SIGTERM', stop)
  process.once('SIGINT', stop)
}
