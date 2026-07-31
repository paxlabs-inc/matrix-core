import { spawn } from 'node:child_process'
import { createServer, request } from 'node:http'

const publicPort = 3000
const upstreamPort = 3010
const marker = 'mx-session=lighthouse-route-proof'

const next = spawn('pnpm', ['start', '--port', String(upstreamPort)], {
  env: process.env,
  stdio: ['ignore', 'pipe', 'pipe'],
})

let proxy
let started = false

function startProxy() {
  if (started) return
  started = true
  proxy = createServer((incoming, outgoing) => {
    const cookies = (incoming.headers.cookie ?? '')
      .split(';')
      .map((cookie) => cookie.trim())
      .filter((cookie) => cookie && !cookie.startsWith('mx-session='))
    cookies.push(marker)

    const upstream = request(
      {
        hostname: '127.0.0.1',
        port: upstreamPort,
        method: incoming.method,
        path: incoming.url,
        headers: {
          ...incoming.headers,
          cookie: cookies.join('; '),
        },
      },
      (response) => {
        outgoing.writeHead(response.statusCode ?? 502, response.headers)
        response.pipe(outgoing)
      },
    )
    upstream.on('error', (error) => {
      if (!outgoing.headersSent) outgoing.writeHead(502)
      outgoing.end(error.message)
    })
    incoming.pipe(upstream)
  })
  proxy.listen(publicPort, '127.0.0.1', () => {
    process.stdout.write('Lighthouse proxy ready\n')
  })
}

next.stdout.on('data', (chunk) => {
  process.stdout.write(chunk)
  if (chunk.toString().toLowerCase().includes('ready in')) startProxy()
})
next.stderr.pipe(process.stderr)
next.on('error', (error) => {
  process.stderr.write(`${error.message}\n`)
  process.exitCode = 1
})
next.on('exit', (code) => {
  if (!started) process.exit(code ?? 1)
})

function shutdown() {
  proxy?.close()
  next.kill('SIGTERM')
}

process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)
