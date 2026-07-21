#!/usr/bin/env node

import { createInterface } from 'node:readline'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const API_BASE = (process.env.MACHINEMAIL_API_URL || 'https://api.machinemail.org/v1').replace(/\/$/, '')
const API_KEY = (process.env.MACHINEMAIL_API_KEY || '').trim()
const TIMEOUT_MS = clampInt(process.env.MACHINEMAIL_TIMEOUT_MS, 30000, 1000, 120000)
const PROTOCOL_VERSION = '2024-11-05'

const tools = [
  tool('mail_list_mailboxes', 'List mailboxes available to this MachineMail identity.', {}, []),
  tool('mail_get_mailbox', 'Get one mailbox and its approval policy.', { mailbox_id: str('Mailbox identifier') }, ['mailbox_id']),
  tool('mail_get_inbox', 'List recent inbox items with previews.', {
    mailbox_id: str('Mailbox identifier'),
    limit: integer('Maximum items to return', 1, 100),
    cursor: str('Pagination cursor'),
  }, ['mailbox_id']),
  tool('mail_get_conversation', 'Read a complete email conversation before replying.', {
    mailbox_id: str('Mailbox identifier'),
    conversation_id: str('Conversation identifier'),
  }, ['mailbox_id', 'conversation_id']),
  tool('mail_compose', 'Compose mail. pending_approval is a successful parked send and must not be retried.', {
    mailbox_id: str('Mailbox identifier'),
    to: stringList('Recipient addresses'),
    cc: stringList('CC addresses'),
    bcc: stringList('BCC addresses'),
    subject: str('Message subject'),
    text: str('Plain-text body'),
    html: str('HTML body'),
    attachments: array('Attachments accepted by MachineMail'),
    send: bool('Request delivery rather than draft creation'),
    approval_scope: str('Approval scope such as draft'),
    idempotency_key: str('Stable key reused for the same logical send'),
  }, ['mailbox_id', 'to', 'subject', 'idempotency_key']),
  tool('mail_reply', 'Reply in an existing conversation. pending_approval is successful parked state.', {
    mailbox_id: str('Mailbox identifier'),
    conversation_id: str('Conversation identifier'),
    text: str('Plain-text body'),
    html: str('HTML body'),
    attachments: array('Attachments accepted by MachineMail'),
    send: bool('Request delivery rather than draft creation'),
    approval_scope: str('Approval scope such as draft'),
    idempotency_key: str('Stable key reused for the same logical reply'),
  }, ['mailbox_id', 'conversation_id', 'idempotency_key']),
  tool('mail_get_threading_headers', 'Get explicit threading headers for a conversation.', {
    mailbox_id: str('Mailbox identifier'),
    conversation_id: str('Conversation identifier'),
  }, ['mailbox_id', 'conversation_id']),
  tool('mail_get_pending_approvals', 'List parked messages awaiting human approval.', {
    mailbox_id: str('Mailbox identifier'),
    limit: integer('Maximum approvals to return', 1, 100),
    cursor: str('Pagination cursor'),
  }, ['mailbox_id']),
  tool('mail_get_approval', 'Get one approval and its authoritative state.', {
    mailbox_id: str('Mailbox identifier'),
    approval_id: str('Approval identifier'),
  }, ['mailbox_id', 'approval_id']),
  tool('mail_search_messages', 'Search messages in a mailbox.', {
    mailbox_id: str('Mailbox identifier'),
    query: str('Search query'),
    limit: integer('Maximum matches to return', 1, 100),
    cursor: str('Pagination cursor'),
  }, ['mailbox_id', 'query']),
  tool('mail_poll_events', 'Read ordered mailbox events after a cursor.', {
    mailbox_id: str('Mailbox identifier'),
    after: str('Last processed event cursor'),
    limit: integer('Maximum events to return', 1, 100),
  }, ['mailbox_id']),
  tool('mail_get_usage', 'Read account usage and limits for preflight.', {}, []),
]

const toolNames = new Set(tools.map((entry) => entry.name))

function str(description) { return { type: 'string', description } }
function bool(description) { return { type: 'boolean', description } }
function integer(description, minimum, maximum) { return { type: 'integer', description, minimum, maximum } }
function array(description) { return { type: 'array', description, items: { type: 'object' } } }
function stringList(description) { return { type: 'array', description, items: { type: 'string' }, minItems: 1 } }
function tool(name, description, properties, required) {
  return { name, description, inputSchema: { type: 'object', properties, required, additionalProperties: false } }
}
function clampInt(value, fallback, minimum, maximum) {
  const parsed = Number.parseInt(value || '', 10)
  return Number.isFinite(parsed) ? Math.min(maximum, Math.max(minimum, parsed)) : fallback
}
function compact(value) {
  return Object.fromEntries(Object.entries(value).filter(([, item]) => item !== undefined && item !== null && item !== ''))
}
function query(params) {
  const search = new URLSearchParams(compact(params))
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}
function mailboxPath(args, suffix = '') {
  return `/mailboxes/${encodeURIComponent(args.mailbox_id)}${suffix}`
}

const operations = {
  mail_list_mailboxes: () => ['GET', '/mailboxes'],
  mail_get_mailbox: (args) => ['GET', mailboxPath(args)],
  mail_get_inbox: (args) => ['GET', mailboxPath(args, `/inbox${query({ limit: args.limit, cursor: args.cursor })}`)],
  mail_get_conversation: (args) => ['GET', mailboxPath(args, `/conversations/${encodeURIComponent(args.conversation_id)}`)],
  mail_compose: (args) => ['POST', mailboxPath(args, '/messages'), omit(args, 'mailbox_id')],
  mail_reply: (args) => ['POST', mailboxPath(args, `/conversations/${encodeURIComponent(args.conversation_id)}/reply`), omit(args, 'mailbox_id', 'conversation_id')],
  mail_get_threading_headers: (args) => ['GET', mailboxPath(args, `/conversations/${encodeURIComponent(args.conversation_id)}/threading-headers`)],
  mail_get_pending_approvals: (args) => ['GET', mailboxPath(args, `/approvals${query({ status: 'pending', limit: args.limit, cursor: args.cursor })}`)],
  mail_get_approval: (args) => ['GET', mailboxPath(args, `/approvals/${encodeURIComponent(args.approval_id)}`)],
  mail_search_messages: (args) => ['GET', mailboxPath(args, `/messages/search${query({ q: args.query, limit: args.limit, cursor: args.cursor })}`)],
  mail_poll_events: (args) => ['GET', mailboxPath(args, `/events${query({ after: args.after, limit: args.limit })}`)],
  mail_get_usage: () => ['GET', '/usage'],
}

function omit(value, ...keys) {
  const removed = new Set(keys)
  return Object.fromEntries(Object.entries(value).filter(([key, item]) => !removed.has(key) && item !== undefined && item !== null && item !== ''))
}
function result(payload, isError = false) {
  return { content: [{ type: 'text', text: JSON.stringify(payload) }], isError }
}
function classify(status, payload) {
  if (status === 401 || status === 403) return 'authorization'
  if (status === 404) return 'not_found'
  if (status === 409) return 'conflict'
  if (status === 422 || status === 400) return 'validation'
  if (status === 429) return 'rate_limit'
  if (status >= 500) return 'service'
  return payload?.error ? 'application' : 'http'
}

async function invoke(name, args) {
  if (!API_KEY) return result({ ok: false, tool: name, class: 'precondition', error: 'MACHINEMAIL_API_KEY is not configured' }, true)
  const operation = operations[name]
  if (!operation) return result({ ok: false, tool: name, class: 'protocol', error: `unknown tool: ${name}` }, true)
  const [method, path, body] = operation(args || {})
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS)
  let response
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method,
      headers: compact({
        Authorization: `Bearer ${API_KEY}`,
        Accept: 'application/json',
        'Content-Type': body === undefined ? undefined : 'application/json',
        'Idempotency-Key': args?.idempotency_key,
      }),
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: controller.signal,
    })
  } catch (error) {
    clearTimeout(timer)
    const timedOut = error?.name === 'AbortError'
    return result({ ok: false, tool: name, class: 'transport', retryable: true, error: timedOut ? `timed out after ${TIMEOUT_MS}ms` : String(error?.message || error) }, true)
  }
  clearTimeout(timer)
  const raw = await response.text()
  let payload
  try { payload = raw ? JSON.parse(raw) : {} } catch { payload = { raw } }
  if (!response.ok) {
    return result({ ok: false, tool: name, class: classify(response.status, payload), status: response.status, retryable: response.status === 429 || response.status >= 500, error: payload?.error || payload?.message || response.statusText, details: payload }, true)
  }
  const state = payload?.status || payload?.state
  return result({ ok: true, tool: name, status: state, pending_approval: state === 'pending_approval', data: payload })
}

const handlers = {
  initialize: (params) => ({ protocolVersion: params?.protocolVersion || PROTOCOL_VERSION, serverInfo: { name: 'machine-mail', version: '0.1.0' }, capabilities: { tools: {} } }),
  'notifications/initialized': () => null,
  'tools/list': () => ({ tools }),
  'tools/call': (params) => invoke(params?.name, params?.arguments || {}),
  ping: () => ({}),
}
function send(value) { process.stdout.write(`${JSON.stringify(value)}\n`) }
function start() {
  const lines = createInterface({ input: process.stdin })
  lines.on('line', async (line) => {
    if (!line.trim()) return
    let request
    try { request = JSON.parse(line) } catch (error) { send({ jsonrpc: '2.0', id: null, error: { code: -32700, message: error.message } }); return }
    const handler = handlers[request.method]
    if (!handler) { if (request.id !== undefined) send({ jsonrpc: '2.0', id: request.id, error: { code: -32601, message: `method not found: ${request.method}` } }); return }
    try {
      const output = await handler(request.params)
      if (request.id !== undefined && output !== null) send({ jsonrpc: '2.0', id: request.id, result: output })
    } catch (error) {
      if (request.id !== undefined) send({ jsonrpc: '2.0', id: request.id, error: { code: -32000, message: String(error?.message || error) } })
    }
  })
}
function selftest() {
  const manifestPath = fileURLToPath(new URL('../../agents/neo.json', import.meta.url))
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
  const declared = new Set(manifest.servers.find((server) => server.alias === 'machine-mail')?.tools.map((entry) => entry.name) || [])
  const missing = [...toolNames].filter((name) => !declared.has(name))
  const extra = [...declared].filter((name) => !toolNames.has(name))
  if (missing.length || extra.length) {
    process.stderr.write(`machine-mail manifest drift missing=${missing.join(',')} extra=${extra.join(',')}\n`)
    process.exit(1)
  }
  process.stdout.write(`machine-mail OK (${tools.length} tools)\n`)
}

if (process.argv.includes('--selftest')) selftest()
else start()
