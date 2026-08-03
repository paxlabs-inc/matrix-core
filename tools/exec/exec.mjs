#!/usr/bin/env node
// exec — MCP stdio bridge giving Matrix agents a real execution surface.
//
// This is the missing primitive that lets the per-user agent handle ANY
// software task end-to-end on its OWN Fly machine: run shell commands,
// install dependencies, build projects, and (crucially) start + supervise
// LONG-LIVED background services (a web server, a Telegram bot, a worker)
// that keep running after the tool call returns and across machine
// restarts.
//
// Pairs with the baked-in `fs` server (write files into /workspace) and
// `git`: `fs` authors the project, `exec` installs/builds/runs it, and the
// service supervisor keeps the result alive.
//
// Tools (6):
//   shell            run a shell command to completion (cwd, env, timeout)
//   service_start    start/replace a supervised long-lived background service
//   service_list     list supervised services (pid, running, uptime, command)
//   service_logs     tail a service's combined stdout+stderr log
//   service_stop     stop a service (SIGTERM → SIGKILL the process group)
//   service_restart  restart a service from its recorded command
//
// Persistence: the service registry + per-service logs live on the Fly
// volume (MATRIX_EXEC_STATE_DIR, default ${MATRIX_DATA_DIR:-/data}/services)
// so services + their logs survive daemon restarts. On boot this server
// re-spawns every service flagged autostart whose recorded pid is dead, so
// the agent's work keeps running across scale-to-zero wakes and machine
// restarts.
//
// Default working directory is /workspace (= the persisted, git-initialised
// /data/workspace), so everything the agent builds lands on the volume.
//
// Wire protocol mirrors tools/websearch/web-search.mjs and
// tools/paxeer/paxeer-net.mjs (newline-delimited JSON-RPC over stdio, zero
// npm deps, Node 18+). Run `node tools/exec/exec.mjs --selftest` to smoke it
// offline (verifies manifest<->bridge tool bijection AND exercises a real
// shell call + a service lifecycle in a temp dir).

import { createInterface } from 'node:readline'
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import {
  readdirSync,
  readFileSync,
  writeFileSync,
  mkdirSync,
  existsSync,
  openSync,
  closeSync,
  readSync,
  statSync,
  renameSync,
  chmodSync,
  readlinkSync,
} from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, isAbsolute, resolve } from 'node:path'
import { tmpdir } from 'node:os'
import { createServer } from 'node:http'

const SERVER_NAME = 'exec'
const SERVER_VERSION = '0.1.0'

// ── config (env-overridable) ──────────────────────────────────────────────────
const DATA_DIR = process.env.MATRIX_DATA_DIR || '/data'
const DEFAULT_STATE_DIR = join(DATA_DIR, 'services')

// Default cwd: the persisted, git-initialised workspace. Falls back sensibly
// when neither /workspace nor /data/workspace exists (dev box / selftest).
const DEFAULT_WORKDIR = resolveDefaultWorkdir()

const SHELL_TIMEOUT_MS = clampInt(process.env.MATRIX_EXEC_TIMEOUT_MS, 120_000, 1_000, 3_600_000)
const MAX_OUTPUT_BYTES = clampInt(process.env.MATRIX_EXEC_MAX_OUTPUT_BYTES, 200_000, 1_000, 5_000_000)
const MAX_SERVICES = clampInt(process.env.MATRIX_EXEC_MAX_SERVICES, 50, 1, 500)
const MAX_LOG_LINES = clampInt(process.env.MATRIX_EXEC_MAX_LOG_LINES, 2_000, 1, 50_000)
const INLINE_SECRET_POLICY = normalizeInlineSecretPolicy(process.env.MATRIX_EXEC_INLINE_SECRET_POLICY)
const PROCESS_IDENTITY_VERSION = 1

function clampInt(v, def, min, max) {
  const n = Number.parseInt(v ?? '', 10)
  if (!Number.isFinite(n)) return def
  return Math.min(max, Math.max(min, n))
}

function normalizeInlineSecretPolicy(value) {
  const policy = String(value || 'report').trim().toLowerCase()
  return policy === 'block' ? 'block' : 'report'
}

function resolveDefaultWorkdir() {
  const candidates = [
    process.env.MATRIX_EXEC_WORKDIR,
    '/workspace',
    join(DATA_DIR, 'workspace'),
  ].filter(Boolean)
  for (const c of candidates) {
    try {
      if (statSync(c).isDirectory()) return c
    } catch {
      /* not present */
    }
  }
  return process.cwd()
}

function stateDir() {
  return process.env.MATRIX_EXEC_STATE_DIR || DEFAULT_STATE_DIR
}

function registryPath() {
  return join(stateDir(), 'registry.json')
}

// ── result shaping ────────────────────────────────────────────────────────────
function result(obj, isError = false) {
  const shaped = { content: [{ type: 'text', text: typeof obj === 'string' ? obj : JSON.stringify(obj) }] }
  if (isError) shaped.isError = true
  return shaped
}
function ok(obj) {
  return result(obj)
}
function fail(tool, error, extra = {}) {
  return result({ ok: false, tool, error, ...extra }, true)
}

// ── state dir + registry ───────────────────────────────────────────────────────
function ensureStateDir() {
  try {
    mkdirSync(stateDir(), { recursive: true })
  } catch {
    /* best effort; falls back to tmp on write failure */
  }
}

function loadRegistry() {
  try {
    return JSON.parse(readFileSync(registryPath(), 'utf8')) || {}
  } catch {
    return {}
  }
}

function saveRegistry(reg) {
  ensureStateDir()
  const path = registryPath()
  const tmp = path + '.tmp'
  writeFileSync(tmp, JSON.stringify(reg, null, 2), { mode: 0o600 })
  renameSync(tmp, path)
  chmodSync(path, 0o600)
}

// ── helpers ─────────────────────────────────────────────────────────────────
function validName(s) {
  return typeof s === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$/.test(s)
}

function resolveCwd(cwd) {
  if (cwd === undefined || cwd === null || cwd === '') return DEFAULT_WORKDIR
  const p = String(cwd)
  const abs = isAbsolute(p) ? p : join(DEFAULT_WORKDIR, p)
  return abs
}

const VISIBLE_PARENT_ENV = new Set([
  'HOME', 'USER', 'LOGNAME', 'SHELL', 'PATH', 'PWD', 'OLDPWD',
  'LANG', 'LANGUAGE', 'LC_ALL', 'LC_CTYPE', 'TERM', 'COLORTERM', 'TZ', 'TMPDIR',
  'CODY_USER_ID', 'MATRIX_USER_ID', 'CODY_PREVIEW_IMAGE',
  'KINDLE_FRONTEND_URL', 'KINDLE_MEDIA_GATEWAY', 'KINDLE_METADATA_URL', 'KINDLE_RPC_URL',
  'MATRIX_BROWSER_URL', 'MATRIX_CHRONOS_TOKEN', 'MATRIX_CHRONOS_URL',
  'MATRIX_COMPILER_ESCALATE_MODEL', 'MATRIX_COMPILER_MODEL', 'MATRIX_DATA_DIR',
  'MATRIX_DEFAULT_SKILL', 'MATRIX_DEUS_TIMEOUT_MS', 'MATRIX_DEUS_URL',
  'MATRIX_EXECUTOR_MODEL', 'MATRIX_GATEWAY_TOKEN', 'MATRIX_GATEWAY_URL',
  'MATRIX_LAYERX_TOKEN', 'MATRIX_LAYERX_URL', 'MATRIX_LIAISON_MODEL',
  'MATRIX_PLANNER_MODEL', 'MATRIX_SEARXNG_TOKEN', 'MATRIX_SEARXNG_URL',
  'MATRIX_SNAPSHOT_INTERVAL', 'MATRIX_TACHYON_TOKEN', 'MATRIX_TACHYON_URL',
  'MATRIX_UWAC_TOKEN', 'MATRIX_UWAC_URL', 'PAXC_API', 'PAXC_TOKEN',
  'WEBSEARCH_PROVIDER', 'NEO_AUTOMATRIX_ENABLED', 'NEO_AUTOMATRIX_INTERVAL',
  'NEO_AUTOMATRIX_JITTER', 'NEO_AUTOMATRIX_MAX_PER_DAY',
  'NEO_AUTOMATRIX_MIN_CONFIDENCE', 'MATRIX_MEDIA_XAI_VIDEO_MODEL',
  'NEO_CONTINUOUS_MEMORY', 'VAULT_REQUIRED', 'VOICE_IDLE_DISCONNECT_S', 'NEO_RUNTIME',
  'MATRIX_EXEC_STATE_DIR', 'MATRIX_EXEC_WORKDIR', 'MATRIX_EXEC_TIMEOUT_MS',
  'MATRIX_EXEC_MAX_OUTPUT_BYTES', 'MATRIX_EXEC_MAX_SERVICES', 'MATRIX_EXEC_MAX_LOG_LINES',
  'MATRIX_EXEC_INLINE_SECRET_POLICY',
])

const PROTECTED_ENV = new Set([
  'BRAVE_API_KEY', 'MATRIX_BROWSER_TOKEN',
  'MATRIX_S3_BUCKET', 'MATRIX_S3_ENDPOINT', 'MATRIX_S3_KEY', 'MATRIX_S3_SECRET',
  'NOVITA_API_KEY', 'RAILWAY_API_TOKEN', 'RAILWAY_ENVIRONMENT_ID',
  'RAILWAY_PROJECT_ID', 'ROUTER_INTERNAL_URL', 'ROUTER_PREVIEW_TOKEN',
  'TAVILY_API_KEY', 'XAI_API_KEY', 'MIMO_API_KEY', 'XIAOMI_API_KEY',
  'MATRIX_LIVEKIT_KEY', 'MATRIX_LIVEKIT_SECRET', 'MATRIX_LIVEKIT_URL',
  'MATRIX_SANDBOX_TOKEN', 'MATRIX_SANDBOX_URL',
  'NEO_VOICE_ENABLED', 'NEO_VOICE_MODE', 'NEO_VOICE_TTS_STYLE',
  'NEO_VOICE_TTS_VOICE', 'NEO_VOICE_ASR_DEADLINE_SECONDS',
  'NEO_VOICE_TTS_DEADLINE_SECONDS', 'ALPHAVANTAGE_API_KEY', 'FMP_API_KEY',
  'ROUTER_FINANCE_TOKEN', 'VAULT_KEK', 'VAULT_KEK_FILE',
  'MATRIX_VAULT_KEK', 'MATRIX_VAULT_KEK_ID',
])

function mergedEnv(extra) {
  const env = {}
  for (const [key, value] of Object.entries(process.env)) {
    if (VISIBLE_PARENT_ENV.has(key)) env[key] = value
  }
  if (extra && typeof extra === 'object') {
    for (const [k, v] of Object.entries(extra)) {
      if (v === null || v === undefined) continue
      if (!VISIBLE_PARENT_ENV.has(k)) throw new Error(`environment override ${k} is not permitted`)
      env[k] = String(v)
    }
  }
  return env
}

function parseProcessStat(text) {
  const close = text.lastIndexOf(') ')
  if (close < 0) return null
  const fields = text.slice(close + 2).trim().split(/\s+/)
  if (fields.length < 20) return null
  const state = fields[0]
  const processGroupID = Number.parseInt(fields[2], 10)
  const startTicks = fields[19]
  if (!Number.isFinite(processGroupID) || !/^\d+$/.test(startTicks)) return null
  return { state, process_group_id: processGroupID, start_ticks: startTicks }
}

function processUID(pid) {
  try {
    const status = readFileSync(`/proc/${pid}/status`, 'utf8')
    const match = status.match(/^Uid:\s+(\d+)/m)
    return match ? Number.parseInt(match[1], 10) : null
  } catch {
    return null
  }
}

function pidNamespace() {
  try {
    return readlinkSync('/proc/self/ns/pid')
  } catch {
    return null
  }
}

function processStartTicks(pid) {
  try {
    return parseProcessStat(readFileSync(`/proc/${pid}/stat`, 'utf8'))?.start_ticks || null
  } catch {
    return null
  }
}

function commandHash(command) {
  return createHash('sha256').update(String(command || '')).digest('hex')
}

export function readProcessIdentity(pid, command) {
  if (!pid || !Number.isFinite(pid)) return null
  try {
    const stat = parseProcessStat(readFileSync(`/proc/${pid}/stat`, 'utf8'))
    if (!stat || stat.state === 'Z' || stat.state === 'X') return null
    return {
      version: PROCESS_IDENTITY_VERSION,
      start_ticks: stat.start_ticks,
      process_group_id: stat.process_group_id,
      pid_namespace: pidNamespace(),
      init_start_ticks: processStartTicks(1),
      uid: processUID(pid),
      command_sha256: commandHash(command),
    }
  } catch {
    return null
  }
}

export function inspectRegisteredProcess(svc) {
  const pid = Number(svc?.pid)
  if (!Number.isFinite(pid) || pid <= 0) {
    return { running: false, state: 'stopped', pid: null }
  }
  const expected = svc?.process_identity
  const observed = readProcessIdentity(pid, svc?.command)
  if (!expected || expected.version !== PROCESS_IDENTITY_VERSION) {
    return {
      running: false,
      state: observed ? 'legacy_unverified' : 'stopped',
      pid,
      reason: observed ? 'stored pid has no verifiable process identity' : 'process is not running',
    }
  }
  if (!observed) {
    return { running: false, state: 'stopped', pid, reason: 'process is not running' }
  }
  const keys = [
    'start_ticks',
    'process_group_id',
    'pid_namespace',
    'init_start_ticks',
    'uid',
    'command_sha256',
  ]
  const mismatch = keys.find((key) => expected[key] !== observed[key])
  if (mismatch) {
    return {
      running: false,
      state: 'pid_alias',
      pid,
      reason: `stored pid belongs to a different process (${mismatch} mismatch)`,
    }
  }
  return { running: true, state: 'running', pid }
}

const INLINE_SECRET_PATTERNS = [
  /(?:^|\s)(?:export\s+)?(?:[A-Za-z_][A-Za-z0-9_]*_(?:TOKEN|SECRET|PASSWORD|KEY)|TOKEN|SECRET|PASSWORD|KEY)\s*=\s*(?!["']?\$)[^\s;&|]+/i,
  /(?:^|\s)--(?:token|api-key|access-token|secret|password)(?:=|\s+)(?!["']?\$)[^\s;&|]+/i,
  /authorization\s*:\s*bearer\s+(?!\$)[^\s"']+/i,
  /[?&](?:token|api_key|access_token|key|secret|password)=(?!\$)[^\s&#"']+/i,
]

function hasInlineSecret(command) {
  return INLINE_SECRET_PATTERNS.some((pattern) => pattern.test(String(command || '')))
}

function redactCommand(command) {
  return String(command || '')
    .replace(
      /((?:^|\s)(?:export\s+)?(?:[A-Za-z_][A-Za-z0-9_]*_(?:TOKEN|SECRET|PASSWORD|KEY)|TOKEN|SECRET|PASSWORD|KEY)\s*=\s*)(?!["']?\$)([^\s;&|]+)/gi,
      '$1[REDACTED]',
    )
    .replace(
      /((?:^|\s)--(?:token|api-key|access-token|secret|password)(?:=|\s+))(?!["']?\$)([^\s;&|]+)/gi,
      '$1[REDACTED]',
    )
    .replace(/(authorization\s*:\s*bearer\s+)(?!\$)([^\s"']+)/gi, '$1[REDACTED]')
    .replace(/([?&](?:token|api_key|access_token|key|secret|password)=)(?!\$)([^\s&#"']+)/gi, '$1[REDACTED]')
}

function logPathFor(name) {
  return join(stateDir(), `${name}.log`)
}

// Tail the last `lines` lines of a file without loading the whole thing when
// it's large. Reads the trailing window (1MB) which is plenty for a tail.
function tailFile(path, lines) {
  if (!existsSync(path)) return ''
  let size = 0
  try {
    size = statSync(path).size
  } catch {
    return ''
  }
  const window = Math.min(size, 1_048_576)
  const buf = Buffer.alloc(window)
  const fd = openSync(path, 'r')
  try {
    readSync(fd, buf, 0, window, size - window)
  } finally {
    closeSync(fd)
  }
  const text = buf.toString('utf8')
  const all = text.split('\n')
  return all.slice(Math.max(0, all.length - lines)).join('\n')
}

// ── service supervisor ──────────────────────────────────────────────────────
// Spawn a detached, log-redirected background process in its own process
// group so it survives this server's lifetime and can be killed as a tree.
function spawnService(name, command, cwd, env) {
  ensureStateDir()
  const logFile = logPathFor(name)
  const fd = openSync(logFile, 'a')
  try {
    const header = `\n=== [matrix-exec] start ${new Date().toISOString()} cwd=${cwd} ===\n`
    writeFileSync(fd, header)
  } catch {
    /* non-fatal */
  }
  const child = spawn('bash', ['-lc', command], {
    cwd,
    env,
    detached: true,
    stdio: ['ignore', fd, fd],
  })
  child.unref()
  closeSync(fd)
  const processIdentity = readProcessIdentity(child.pid, command)
  if (!processIdentity) {
    try {
      process.kill(-child.pid, 'SIGKILL')
    } catch {
      try {
        child.kill('SIGKILL')
      } catch {
        /* gone */
      }
    }
    throw new Error('spawned process identity could not be captured')
  }
  return { pid: child.pid, process_identity: processIdentity }
}

function signalRegisteredProcess(svc, signal) {
  const state = inspectRegisteredProcess(svc)
  if (!state.running) return false
  const pid = state.pid
  // Kill the whole process group (negative pid) since we spawned detached.
  try {
    process.kill(-pid, signal)
  } catch {
    try {
      process.kill(pid, signal)
    } catch {
      /* gone */
    }
  }
  return true
}

export function stopRegisteredProcess(svc) {
  if (!inspectRegisteredProcess(svc).running) return false
  signalRegisteredProcess(svc, 'SIGTERM')
  // Give it a moment, then hard-kill if still alive.
  const deadline = Date.now() + 3_000
  // Busy-wait is acceptable here (short, bounded, the daemon is async around us).
  const sleep = (ms) => {
    const end = Date.now() + ms
    while (Date.now() < end) {
      /* spin */
    }
  }
  while (Date.now() < deadline && inspectRegisteredProcess(svc).running) sleep(50)
  if (inspectRegisteredProcess(svc).running) signalRegisteredProcess(svc, 'SIGKILL')
  return true
}

function clearObservedProcess(svc, state) {
  svc.pid = null
  delete svc.process_identity
  svc.status = 'stopped'
  svc.stopped_at = new Date().toISOString()
  if (state?.state === 'pid_alias' || state?.state === 'legacy_unverified') {
    svc.last_error = state.reason
  }
}

function applySpawn(svc, spawned) {
  svc.pid = spawned.pid
  svc.process_identity = spawned.process_identity
  svc.status = 'running'
  svc.started_at = new Date().toISOString()
  svc.last_reconciled_at = svc.started_at
  delete svc.stopped_at
  delete svc.last_error
}

function inlineSecretWarning(command) {
  return hasInlineSecret(command) ? 'inline credential material detected in persisted command' : ''
}

// On boot, invalidate every unverified or stale pid, then re-spawn autostart
// services exactly once from their persisted desired configuration.
export function respawnAutostart() {
  const reg = loadRegistry()
  let changed = false
  const summary = { checked: 0, running: 0, invalidated: 0, respawned: 0, blocked: 0, failed: 0 }
  for (const [name, svc] of Object.entries(reg)) {
    if (!svc || typeof svc !== 'object') continue
    summary.checked++
    const observed = inspectRegisteredProcess(svc)
    svc.last_reconciled_at = new Date().toISOString()
    if (observed.running) {
      svc.status = 'running'
      summary.running++
      changed = true
      continue
    }
    if (svc.pid || svc.process_identity) {
      clearObservedProcess(svc, observed)
      summary.invalidated++
      changed = true
    }
    if (svc.autostart === false) continue
    const warning = inlineSecretWarning(svc.command)
    if (warning) {
      svc.security_warning = warning
      changed = true
      if (INLINE_SECRET_POLICY === 'block') {
        svc.last_error = `${warning}; autostart blocked by policy`
        svc.status = 'blocked'
        summary.blocked++
        continue
      }
    } else {
      delete svc.security_warning
    }
    try {
      const spawned = spawnService(name, svc.command, svc.cwd || DEFAULT_WORKDIR, mergedEnv(svc.env))
      applySpawn(svc, spawned)
      summary.respawned++
      changed = true
    } catch (e) {
      svc.last_error = e?.message ?? String(e)
      svc.status = 'failed'
      summary.failed++
      changed = true
    }
  }
  if (changed) {
    try {
      saveRegistry(reg)
    } catch {
      /* best effort */
    }
  }
  return summary
}

// ── tool: shell ───────────────────────────────────────────────────────────────
function runShell(args) {
  const command = (args?.command ?? '').toString()
  if (!command.trim()) return Promise.resolve(fail('shell', 'command is required'))
  const cwd = resolveCwd(args?.cwd)
  if (!existsSync(cwd)) return Promise.resolve(fail('shell', `cwd does not exist: ${cwd}`))
  const timeout = clampInt(args?.timeout_ms, SHELL_TIMEOUT_MS, 1_000, 3_600_000)
  let env
  try {
    env = mergedEnv(args?.env)
  } catch (e) {
    return Promise.resolve(fail('shell', e?.message ?? String(e), { cwd }))
  }

  return new Promise((resolve) => {
    let child
    try {
      const strictCommand = `curl() { command curl --fail-with-body "$@"; }\nexport -f curl\n${command}`
      child = spawn('bash', ['-lc', strictCommand], { cwd, env, detached: true })
    } catch (e) {
      resolve(fail('shell', `spawn failed: ${e?.message ?? String(e)}`, { cwd }))
      return
    }
    const t0 = Date.now()
    let out = Buffer.alloc(0)
    let err = Buffer.alloc(0)
    let outTrunc = false
    let errTrunc = false
    let timedOut = false

    const cap = (buf, chunk, trunc) => {
      if (trunc) return [buf, true]
      const room = MAX_OUTPUT_BYTES - buf.length
      if (room <= 0) return [buf, true]
      if (chunk.length <= room) return [Buffer.concat([buf, chunk]), false]
      return [Buffer.concat([buf, chunk.subarray(0, room)]), true]
    }

    child.stdout.on('data', (c) => {
      ;[out, outTrunc] = cap(out, c, outTrunc)
    })
    child.stderr.on('data', (c) => {
      ;[err, errTrunc] = cap(err, c, errTrunc)
    })

    const timer = setTimeout(() => {
      timedOut = true
      try {
        process.kill(-child.pid, 'SIGKILL')
      } catch {
        try {
          child.kill('SIGKILL')
        } catch {
          /* gone */
        }
      }
    }, timeout)

    child.on('error', (e) => {
      clearTimeout(timer)
      resolve(fail('shell', `exec error: ${e?.message ?? String(e)}`, { cwd }))
    })

    child.on('close', (code, signal) => {
      clearTimeout(timer)
      const result = {
        ok: code === 0 && !timedOut,
        tool: 'shell',
        exit_code: code,
        signal: signal || null,
        timed_out: timedOut,
        duration_ms: Date.now() - t0,
        cwd,
        stdout: out.toString('utf8'),
        stderr: err.toString('utf8'),
        stdout_truncated: outTrunc,
        stderr_truncated: errTrunc,
      }
      const statusMatch = result.stderr.match(/requested URL returned error:\s*(\d{3})/i)
      if (statusMatch) result.http_status = Number.parseInt(statusMatch[1], 10)
      resolve(result.ok ? ok(result) : fail('shell', timedOut ? 'process timed out' : `process exited ${code}`, result))
    })
  })
}

// ── tool: service_start ─────────────────────────────────────────────────────
function serviceStart(args) {
  const name = args?.name
  if (!validName(name)) return fail('service_start', 'name is required (letters, digits, _.-, ≤64 chars)')
  const command = (args?.command ?? '').toString()
  if (!command.trim()) return fail('service_start', 'command is required')
  const cwd = resolveCwd(args?.cwd)
  if (!existsSync(cwd)) return fail('service_start', `cwd does not exist: ${cwd}`)
  const autostart = args?.autostart !== false
  const env = args?.env && typeof args.env === 'object' ? args.env : undefined
  const securityWarning = inlineSecretWarning(command)
  if (securityWarning && INLINE_SECRET_POLICY === 'block') {
    return fail('service_start', `${securityWarning}; use an approved environment or secret broker reference`)
  }

  const reg = loadRegistry()
  // Replace semantics: if a service with this name is running, stop it first.
  if (reg[name] && inspectRegisteredProcess(reg[name]).running) stopRegisteredProcess(reg[name])
  if (!reg[name] && Object.keys(reg).length >= MAX_SERVICES) {
    return fail('service_start', `service limit reached (${MAX_SERVICES}); stop one first`)
  }

  let spawned
  try {
    spawned = spawnService(name, command, cwd, mergedEnv(env))
  } catch (e) {
    return fail('service_start', `spawn failed: ${e?.message ?? String(e)}`, { name, cwd })
  }
  reg[name] = {
    name,
    command,
    cwd,
    env: env || null,
    autostart,
    pid: spawned.pid,
    process_identity: spawned.process_identity,
    status: 'running',
    started_at: new Date().toISOString(),
    log_file: logPathFor(name),
  }
  if (securityWarning) reg[name].security_warning = securityWarning
  saveRegistry(reg)
  return ok({
    ok: true,
    tool: 'service_start',
    name,
    pid: spawned.pid,
    status: 'running',
    autostart,
    cwd,
    log_file: logPathFor(name),
    security_warning: securityWarning || undefined,
    hint: 'use service_logs to read output; service_list to check status',
  })
}

// ── tool: service_list ────────────────────────────────────────────────────────
function serviceList() {
  const reg = loadRegistry()
  const now = Date.now()
  const services = Object.values(reg).map((svc) => {
    const observed = inspectRegisteredProcess(svc)
    const running = observed.running
    let uptime_s = null
    if (running && svc.started_at) {
      const started = Date.parse(svc.started_at)
      if (Number.isFinite(started)) uptime_s = Math.max(0, Math.round((now - started) / 1000))
    }
    return {
      name: svc.name,
      running,
      status: running ? 'running' : observed.state,
      pid: svc.pid || null,
      identity_verified: running,
      uptime_s,
      autostart: svc.autostart !== false,
      command: redactCommand(svc.command),
      cwd: svc.cwd,
      log_file: svc.log_file || logPathFor(svc.name),
      last_error: svc.last_error ? redactCommand(svc.last_error) : undefined,
      security_warning: svc.security_warning || inlineSecretWarning(svc.command) || undefined,
      stale_reason: running ? undefined : observed.reason,
    }
  })
  return ok({ ok: true, tool: 'service_list', count: services.length, services })
}

export function auditRegistry() {
  const reg = loadRegistry()
  const services = Object.values(reg)
    .filter((svc) => svc && typeof svc === 'object')
    .map((svc) => {
      const observed = inspectRegisteredProcess(svc)
      const state = observed.running
        ? 'running'
        : ['blocked', 'failed'].includes(svc.status)
          ? svc.status
          : observed.state
      return {
        name: svc.name,
        autostart: svc.autostart !== false,
        state,
        pid: svc.pid || null,
        identity_verified: observed.running,
        inline_credential_material: hasInlineSecret(svc.command),
        command: redactCommand(svc.command),
        stale_reason: observed.running ? undefined : observed.reason,
        last_error: svc.last_error ? redactCommand(svc.last_error) : undefined,
      }
    })
    .sort((a, b) => String(a.name).localeCompare(String(b.name)))
  const count = (predicate) => services.filter(predicate).length
  return {
    ok: true,
    state_dir: stateDir(),
    policy: { inline_secret: INLINE_SECRET_POLICY },
    summary: {
      services: services.length,
      running_verified: count((svc) => svc.identity_verified),
      stopped: count((svc) => svc.state === 'stopped'),
      legacy_unverified: count((svc) => svc.state === 'legacy_unverified'),
      pid_aliases: count((svc) => svc.state === 'pid_alias'),
      inline_credential_commands: count((svc) => svc.inline_credential_material),
      autostart_not_running: count((svc) => svc.autostart && !svc.identity_verified),
    },
    services,
  }
}

// ── tool: service_logs ────────────────────────────────────────────────────────
function serviceLogs(args) {
  const name = args?.name
  if (!validName(name)) return fail('service_logs', 'name is required')
  const reg = loadRegistry()
  const svc = reg[name]
  const path = (svc && svc.log_file) || logPathFor(name)
  const lines = clampInt(args?.lines, 200, 1, MAX_LOG_LINES)
  if (!existsSync(path)) return fail('service_logs', `no log for service ${name}`, { name, log_file: path })
  const text = tailFile(path, lines)
  return ok({
    ok: true,
    tool: 'service_logs',
    name,
    log_file: path,
    running: svc ? inspectRegisteredProcess(svc).running : false,
    lines: text,
  })
}

// ── tool: service_stop ────────────────────────────────────────────────────────
function serviceStop(args) {
  const name = args?.name
  if (!validName(name)) return fail('service_stop', 'name is required')
  const reg = loadRegistry()
  const svc = reg[name]
  if (!svc) return fail('service_stop', `unknown service ${name}`)
  const observed = inspectRegisteredProcess(svc)
  const was = observed.running
  if (was) stopRegisteredProcess(svc)
  // Explicit stop disables autostart so it stays down across restarts until
  // the agent restarts it intentionally.
  svc.autostart = false
  svc.pid = null
  delete svc.process_identity
  svc.status = 'stopped'
  svc.stopped_at = new Date().toISOString()
  saveRegistry(reg)
  return ok({ ok: true, tool: 'service_stop', name, was_running: was, status: 'stopped' })
}

// ── tool: service_restart ─────────────────────────────────────────────────────
function serviceRestart(args) {
  const name = args?.name
  if (!validName(name)) return fail('service_restart', 'name is required')
  const reg = loadRegistry()
  const svc = reg[name]
  if (!svc) return fail('service_restart', `unknown service ${name}`)
  if (inspectRegisteredProcess(svc).running) stopRegisteredProcess(svc)
  const cwd = svc.cwd && existsSync(svc.cwd) ? svc.cwd : DEFAULT_WORKDIR
  const securityWarning = inlineSecretWarning(svc.command)
  if (securityWarning && INLINE_SECRET_POLICY === 'block') {
    svc.pid = null
    delete svc.process_identity
    svc.status = 'blocked'
    svc.security_warning = securityWarning
    svc.last_error = `${securityWarning}; restart blocked by policy`
    saveRegistry(reg)
    return fail('service_restart', svc.last_error, { name })
  }
  let spawned
  try {
    spawned = spawnService(name, svc.command, cwd, mergedEnv(svc.env))
  } catch (e) {
    return fail('service_restart', `spawn failed: ${e?.message ?? String(e)}`, { name })
  }
  svc.cwd = cwd
  svc.autostart = true
  applySpawn(svc, spawned)
  if (securityWarning) svc.security_warning = securityWarning
  else delete svc.security_warning
  saveRegistry(reg)
  return ok({
    ok: true,
    tool: 'service_restart',
    name,
    pid: spawned.pid,
    status: 'running',
    log_file: logPathFor(name),
    security_warning: securityWarning || undefined,
  })
}

// ── dispatch ─────────────────────────────────────────────────────────────────
export async function dispatch(name, args = {}) {
  switch (name) {
    case 'shell':
      return runShell(args)
    case 'service_start':
      return serviceStart(args)
    case 'service_list':
      return serviceList()
    case 'service_logs':
      return serviceLogs(args)
    case 'service_stop':
      return serviceStop(args)
    case 'service_restart':
      return serviceRestart(args)
    default:
      throw new Error(`unknown tool: ${name}`)
  }
}

// ── tool descriptors (advertised to the MCP client; MUST match the manifest) ──
const A = (props, required = []) => ({ type: 'object', properties: props, required })
const S = (description) => ({ type: 'string', description })
const N = (description) => ({ type: 'number', description })
const B = (description) => ({ type: 'boolean', description })
const O = (description) => ({ type: 'object', description, additionalProperties: { type: 'string' } })

export const tools = [
  {
    name: 'shell',
    description:
      'Run a shell command (bash -lc) to completion on this machine and return its exit code, stdout, and stderr. Use this to install dependencies (npm/pip/apt), build projects, run scripts, inspect the filesystem, and any one-shot task. Runs in /workspace by default (persisted). For a process that must keep running (a server/bot/worker) use service_start instead. args: command (required), cwd? (default /workspace), timeout_ms? (default 120000), env? (object of extra env vars).',
    inputSchema: A(
      {
        command: S('the shell command to run, e.g. "npm install && npm run build"'),
        cwd: S('working directory; absolute, or relative to /workspace. Default /workspace'),
        timeout_ms: N('kill the command after this many ms (default 120000, max 3600000)'),
        env: O('extra environment variables to set for this command'),
      },
      ['command'],
    ),
  },
  {
    name: 'service_start',
    description:
      'Start (or replace) a long-lived background service that keeps running after this call returns and is automatically restarted on machine reboot. Use for web servers, bots, workers, schedulers. Output is captured to a log readable via service_logs. Starting an existing name replaces the running process with the new command. args: name (required, unique handle), command (required), cwd? (default /workspace), env? (object), autostart? (default true — respawn on machine restart).',
    inputSchema: A(
      {
        name: S('unique service handle, e.g. "telegram-bot" (letters, digits, _.-)'),
        command: S('the command to run as the service, e.g. "node bot.js"'),
        cwd: S('working directory; absolute, or relative to /workspace. Default /workspace'),
        env: O('extra environment variables for the service'),
        autostart: B('respawn automatically on machine restart (default true)'),
      },
      ['name', 'command'],
    ),
  },
  {
    name: 'service_list',
    description:
      'List all supervised services with their status (running/stopped), pid, uptime, command, working directory, and log file path. Read-only.',
    inputSchema: A({}, []),
  },
  {
    name: 'service_logs',
    description:
      "Read the tail of a service's combined stdout+stderr log. Use this to verify a service started correctly or to debug it. Read-only. args: name (required), lines? (default 200).",
    inputSchema: A(
      {
        name: S('the service handle passed to service_start'),
        lines: N('number of trailing log lines to return (default 200)'),
      },
      ['name'],
    ),
  },
  {
    name: 'service_stop',
    description:
      'Stop a supervised service (SIGTERM, then SIGKILL its process group). Disables autostart so it stays down across restarts until restarted intentionally. args: name (required).',
    inputSchema: A({ name: S('the service handle to stop') }, ['name']),
  },
  {
    name: 'service_restart',
    description:
      'Restart a supervised service from its recorded command and re-enable autostart. args: name (required).',
    inputSchema: A({ name: S('the service handle to restart') }, ['name']),
  },
]

export const TOOL_NAMES = tools.map((t) => t.name)

// ── JSON-RPC stdio server ─────────────────────────────────────────────────────
const handlers = {
  initialize: (params) => ({
    protocolVersion: params?.protocolVersion ?? '2024-11-05',
    serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    capabilities: { tools: {} },
  }),
  'tools/list': () => ({ tools }),
  'tools/call': async (params) => {
    const name = params?.name
    const args = params?.arguments || {}
    try {
      return await dispatch(name, args)
    } catch (err) {
      return fail(name, err?.message ?? String(err))
    }
  },
  'notifications/initialized': () => null,
  ping: () => ({}),
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n')
}
const rpcOk = (id, result) => ({ jsonrpc: '2.0', id, result })
const rpcErr = (id, code, message) => ({ jsonrpc: '2.0', id, error: { code, message } })

function startStdioServer() {
  // On boot, re-adopt the agent's persisted services (autostart respawn).
  try {
    respawnAutostart()
  } catch (e) {
    process.stderr.write(`exec: respawn-autostart warning: ${e?.message ?? e}\n`)
  }
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

// ── --selftest ────────────────────────────────────────────────────────────────
// 1) Verify the bridge registry exactly matches every agent manifest that
//    declares an `exec` server (executor/mcp Manager.verifyTools turns drift
//    into a FATAL daemon boot; this catches it at build/CI as a non-zero exit).
// 2) Exercise a real shell call + a full service lifecycle in a temp state dir
//    so the execution path itself is proven offline (no network).
// EXEC_AGENTS_DIR overrides the manifest dir (used by tests).
async function runSelftest() {
  console.log(`exec: ${tools.length} tools`)
  for (const t of tools) console.log(`  - ${t.name}`)

  // (1) manifest bijection
  const bridge = new Set(TOOL_NAMES)
  const agentsDir = process.env.EXEC_AGENTS_DIR ?? fileURLToPath(new URL('../../agents/', import.meta.url))
  let files
  try {
    files = readdirSync(agentsDir).filter((f) => f.endsWith('.json'))
  } catch (err) {
    console.error(`exec SELFTEST FAILED: cannot read agents dir ${agentsDir}: ${err.message}`)
    process.exit(1)
  }
  let checked = 0
  let drift = false
  for (const file of files) {
    let doc
    try {
      doc = JSON.parse(readFileSync(join(agentsDir, file), 'utf8'))
    } catch (err) {
      console.error(`exec FAIL: ${file} is not valid JSON: ${err.message}`)
      drift = true
      continue
    }
    const server = (doc.servers || []).find((s) => s.alias === 'exec')
    if (!server) continue
    checked++
    const declared = new Set((server.tools || []).map((t) => t.name))
    const bridgeOnly = [...bridge].filter((n) => !declared.has(n))
    const manifestOnly = [...declared].filter((n) => !bridge.has(n))
    if (bridgeOnly.length || manifestOnly.length) {
      drift = true
      console.error(`exec FAIL: ${file} drifts from the bridge registry`)
      if (bridgeOnly.length) console.error(`  bridge advertises, manifest omits (boot "unexpected tool"): ${bridgeOnly.join(', ')}`)
      if (manifestOnly.length) console.error(`  manifest expects, bridge omits (boot "missing expected tool"): ${manifestOnly.join(', ')}`)
    } else {
      console.log(`exec: ${file} matches (${declared.size} tools)`)
    }
  }
  if (checked === 0) {
    console.error(`exec SELFTEST FAILED: no manifest under ${agentsDir} declares an exec server`)
    process.exit(1)
  }
  if (drift) {
    console.error('exec SELFTEST FAILED: manifest drift would crash the daemon at boot (Manager.verifyTools)')
    process.exit(1)
  }

  // (2) behavioural smoke in an isolated temp state dir
  const probe = join(tmpdir(), `exec-selftest-${process.pid}-${Date.now()}`)
  mkdirSync(probe, { recursive: true })
  process.env.MATRIX_EXEC_STATE_DIR = join(probe, 'services')
  process.env.MATRIX_EXEC_WORKDIR = probe
  process.env.TAVILY_API_KEY = 'exec-hidden-sentinel'
  process.env.UNREVIEWED_TOKEN = 'exec-unknown-sentinel'
  process.env.MATRIX_USER_ID = 'exec-visible-sentinel'

  // a) shell echo
  const sh = await dispatch('shell', { command: 'echo matrix-exec-ok && pwd && env', cwd: probe })
  const shText = sh.content[0].text
  if (
    !shText.includes('matrix-exec-ok') ||
    !shText.includes('MATRIX_USER_ID=exec-visible-sentinel') ||
    shText.includes('exec-hidden-sentinel') ||
    shText.includes('exec-unknown-sentinel')
  ) {
    console.error('exec SELFTEST FAILED: shell did not return expected output:', shText)
    process.exit(1)
  }
  const protectedOverride = await dispatch('shell', {
    command: 'true',
    cwd: probe,
    env: { TAVILY_API_KEY: 'exec-override-sentinel' },
  })
  const protectedText = protectedOverride.content[0].text
  if (protectedOverride.isError !== true || protectedText.includes('exec-override-sentinel')) {
    console.error('exec SELFTEST FAILED: protected environment override was accepted')
    process.exit(1)
  }

  const unknownOverride = await dispatch('shell', {
    command: 'true',
    cwd: probe,
    env: { UNREVIEWED_TOKEN: 'exec-unknown-override-sentinel' },
  })
  const unknownText = JSON.stringify(unknownOverride)
  if (unknownOverride.isError !== true || unknownText.includes('exec-unknown-override-sentinel')) {
    console.error('exec SELFTEST FAILED: unknown environment override was accepted')
    process.exit(1)
  }
  console.log('exec: shell smoke OK')

  // b) real loopback HTTP + application-envelope semantics
  const http = createServer((req, res) => {
    res.setHeader('content-type', 'application/json')
    if (req.url === '/http-error') {
      res.statusCode = 400
      res.end(JSON.stringify({ ok: false, error: 'bad request' }))
      return
    }
    if (req.url === '/app-error') {
      res.end(JSON.stringify({ ok: false, error: 'application rejected request' }))
      return
    }
    res.end(JSON.stringify({ ok: true, value: 'real-success' }))
  })
  await new Promise((resolve, reject) => {
    http.once('error', reject)
    http.listen(0, '127.0.0.1', resolve)
  })
  try {
    const address = http.address()
    const base = `http://127.0.0.1:${address.port}`
    const httpFailure = await dispatch('shell', { command: `curl -sS ${base}/http-error`, cwd: probe })
    const httpEnvelope = JSON.parse(httpFailure.content[0].text)
    if (httpFailure.isError !== true || httpEnvelope.http_status !== 400 || httpEnvelope.exit_code === 0) {
      console.error('exec SELFTEST FAILED: HTTP 400 was not a failed shell result:', httpFailure)
      process.exit(1)
    }
    const appResponse = await dispatch('shell', { command: `curl -sS ${base}/app-error`, cwd: probe })
    const appEnvelope = JSON.parse(appResponse.content[0].text)
    if (appResponse.isError === true || appEnvelope.exit_code !== 0 || !appEnvelope.stdout.includes('"ok":false')) {
      console.error('exec SELFTEST FAILED: application envelope transport shape changed:', appResponse)
      process.exit(1)
    }
    const success = await dispatch('shell', { command: `curl -sS ${base}/success`, cwd: probe })
    const successEnvelope = JSON.parse(success.content[0].text)
    if (success.isError === true || successEnvelope.exit_code !== 0 || !successEnvelope.stdout.includes('real-success')) {
      console.error('exec SELFTEST FAILED: genuine HTTP success was not preserved:', success)
      process.exit(1)
    }
  } finally {
    await new Promise((resolve) => http.close(resolve))
  }
  console.log('exec: HTTP/process semantics OK')

  // c) long-lived service environment and override policy
  const service = serviceStart({
    name: 'env-probe',
    command: 'env; sleep 5',
    cwd: probe,
    autostart: false,
  })
  if (service.isError === true) {
    console.error('exec SELFTEST FAILED: service did not start:', service)
    process.exit(1)
  }
  let serviceText = ''
  for (let i = 0; i < 40; i++) {
    await new Promise((resolve) => setTimeout(resolve, 25))
    const logs = serviceLogs({ name: 'env-probe', lines: 200 })
    serviceText = logs.content[0].text
    if (serviceText.includes('MATRIX_USER_ID=exec-visible-sentinel')) break
  }
  serviceStop({ name: 'env-probe' })
  if (
    !serviceText.includes('MATRIX_USER_ID=exec-visible-sentinel') ||
    serviceText.includes('exec-hidden-sentinel') ||
    serviceText.includes('exec-unknown-sentinel')
  ) {
    console.error('exec SELFTEST FAILED: supervised service environment was not isolated')
    process.exit(1)
  }
  const unknownServiceOverride = serviceStart({
    name: 'bad-env-probe',
    command: 'true',
    cwd: probe,
    env: { UNREVIEWED_TOKEN: 'exec-service-override-sentinel' },
  })
  const unknownServiceText = JSON.stringify(unknownServiceOverride)
  if (unknownServiceOverride.isError !== true || unknownServiceText.includes('exec-service-override-sentinel')) {
    console.error('exec SELFTEST FAILED: supervised service accepted an unknown environment override')
    process.exit(1)
  }
  console.log('exec: service environment smoke OK')

  console.log(`exec OK (${checked} manifest${checked === 1 ? '' : 's'} verified)`)
  process.exit(0)
}

const isDirect = Boolean(process.argv[1]) && resolve(process.argv[1]) === fileURLToPath(import.meta.url)

if (isDirect) {
  if (process.argv.includes('--selftest')) {
    runSelftest()
  } else if (process.argv.includes('--audit-registry')) {
    process.stdout.write(JSON.stringify(auditRegistry(), null, 2) + '\n')
  } else if (process.argv.includes('--verify-registry')) {
    const audit = auditRegistry()
    process.stdout.write(JSON.stringify(audit, null, 2) + '\n')
    if (
      audit.summary.pid_aliases > 0 ||
      audit.summary.legacy_unverified > 0 ||
      audit.summary.autostart_not_running > 0
    ) {
      process.exitCode = 1
    }
  } else if (process.argv.includes('--verify-no-inline-credentials')) {
    const audit = auditRegistry()
    process.stdout.write(JSON.stringify(audit, null, 2) + '\n')
    if (audit.summary.inline_credential_commands > 0) process.exitCode = 1
  } else {
    startStdioServer()
  }
}
