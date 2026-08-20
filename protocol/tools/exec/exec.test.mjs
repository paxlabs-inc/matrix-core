import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test, { after } from 'node:test'

const probe = mkdtempSync(join(tmpdir(), 'matrix-exec-identity-'))
const stateDir = join(probe, 'services')
mkdirSync(stateDir, { recursive: true })
process.env.MATRIX_EXEC_STATE_DIR = stateDir
process.env.MATRIX_EXEC_WORKDIR = probe
process.env.MATRIX_DATA_DIR = probe

const bridgePath = new URL('./exec.mjs', import.meta.url)
const bridge = await import(`${bridgePath.href}?test=${Date.now()}`)
after(() => rmSync(probe, { recursive: true, force: true }))

function parseResult(result) {
  return JSON.parse(result.content[0].text)
}

async function realSleeper(command = 'real-sleeper') {
  const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], {
    detached: true,
    stdio: 'ignore',
  })
  child.unref()
  await new Promise((resolve, reject) => {
    child.once('spawn', resolve)
    child.once('error', reject)
  })
  const identity = bridge.readProcessIdentity(child.pid, command)
  assert.ok(identity, 'real process identity was not readable')
  return { child, service: { pid: child.pid, command, process_identity: identity } }
}

function processExists(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

test('verified process identity rejects stale aliases without killing unrelated processes', async (t) => {
  const unrelated = await realSleeper('unrelated-process')
  const stale = {
    ...unrelated.service,
    process_identity: { ...unrelated.service.process_identity, start_ticks: '0' },
  }
  assert.equal(bridge.inspectRegisteredProcess(stale).state, 'pid_alias')
  assert.equal(bridge.stopRegisteredProcess(stale), false)
  assert.equal(processExists(unrelated.child.pid), true, 'stale registry entry killed an unrelated process')
  assert.equal(bridge.stopRegisteredProcess(unrelated.service), true)

  const started = parseResult(
    await bridge.dispatch('service_start', {
      name: 'verified-service',
      command: `${process.execPath} -e "setInterval(() => {}, 1000)"`,
      cwd: probe,
      autostart: false,
    }),
  )
  assert.equal(started.ok, true)
  const registry = JSON.parse(readFileSync(join(stateDir, 'registry.json'), 'utf8'))
  assert.ok(registry['verified-service'].process_identity?.start_ticks)
  const listed = parseResult(await bridge.dispatch('service_list'))
  assert.equal(listed.services[0].identity_verified, true)
  assert.equal(listed.services[0].status, 'running')
  const stopped = parseResult(await bridge.dispatch('service_stop', { name: 'verified-service' }))
  assert.equal(stopped.was_running, true)

  const secondUnrelated = await realSleeper('second-unrelated')
  writeFileSync(
    join(stateDir, 'registry.json'),
    JSON.stringify({
      resumed: {
        name: 'resumed',
        command: `${process.execPath} -e "setInterval(() => {}, 1000)"`,
        cwd: probe,
        autostart: true,
        pid: secondUnrelated.child.pid,
      },
    }),
  )
  const reconciliation = bridge.respawnAutostart()
  assert.equal(reconciliation.invalidated, 1)
  assert.equal(reconciliation.respawned, 1)
  const reconciled = JSON.parse(readFileSync(join(stateDir, 'registry.json'), 'utf8')).resumed
  assert.notEqual(reconciled.pid, secondUnrelated.child.pid)
  assert.ok(reconciled.process_identity?.start_ticks)
  assert.equal(processExists(secondUnrelated.child.pid), true, 'boot reconciliation killed an unrelated process')
  bridge.stopRegisteredProcess(secondUnrelated.service)
  parseResult(await bridge.dispatch('service_stop', { name: 'resumed' }))
})

test('registry audit redacts inline credential material', () => {
  writeFileSync(
    join(stateDir, 'registry.json'),
    JSON.stringify({
      unsafe: {
        name: 'unsafe',
        command: 'TELEGRAM_BOT_TOKEN=do-not-render node bot.js --token second-secret',
        cwd: probe,
        autostart: false,
      },
    }),
  )
  const audit = bridge.auditRegistry()
  const rendered = JSON.stringify(audit)
  assert.equal(audit.summary.inline_credential_commands, 1)
  assert.doesNotMatch(rendered, /do-not-render|second-secret/)
  assert.match(rendered, /REDACTED/)
})

test('block policy refuses a new credential-bearing service command', () => {
  const blockedState = join(probe, 'blocked-services')
  const request = JSON.stringify({
    jsonrpc: '2.0',
    id: 1,
    method: 'tools/call',
    params: {
      name: 'service_start',
      arguments: {
        name: 'blocked',
        command: 'API_KEY=do-not-store node server.js',
        cwd: probe,
      },
    },
  })
  const result = spawnSync(process.execPath, [fileURLToPath(bridgePath)], {
    cwd: probe,
    env: {
      ...process.env,
      MATRIX_EXEC_STATE_DIR: blockedState,
      MATRIX_EXEC_WORKDIR: probe,
      MATRIX_EXEC_INLINE_SECRET_POLICY: 'block',
    },
    input: request + '\n',
    encoding: 'utf8',
    timeout: 5_000,
  })
  assert.equal(result.status, 0, result.stderr)
  assert.doesNotMatch(result.stdout, /do-not-store/)
  const response = JSON.parse(result.stdout.trim())
  assert.equal(response.result.isError, true)
  assert.match(response.result.content[0].text, /inline credential material detected/)
})

test('junk cleanup removes only stopped logs and allowlisted cache roots', async () => {
  rmSync(stateDir, { recursive: true, force: true })
  mkdirSync(stateDir, { recursive: true })
  const running = parseResult(
    await bridge.dispatch('service_start', {
      name: 'running-log',
      command: `${process.execPath} -e "setInterval(() => {}, 1000)"`,
      cwd: probe,
      autostart: false,
    }),
  )
  assert.equal(running.ok, true)
  writeFileSync(join(stateDir, 'stopped.log'), 'old output')
  mkdirSync(join(probe, 'tmp'), { recursive: true })
  mkdirSync(join(probe, 'neo', 'cache'), { recursive: true })
  writeFileSync(join(probe, 'tmp', 'scratch'), 'junk')
  writeFileSync(join(probe, 'neo', 'cache', 'scratch'), 'junk')

  const cleaned = bridge.cleanEnvironmentJunk()
  assert.equal(cleaned.ok, true)
  assert.equal(existsSync(join(stateDir, 'stopped.log')), false)
  assert.equal(existsSync(join(stateDir, 'running-log.log')), true)
  assert.equal(existsSync(join(probe, 'tmp', 'scratch')), false)
  assert.equal(existsSync(join(probe, 'neo', 'cache', 'scratch')), false)
  parseResult(await bridge.dispatch('service_stop', { name: 'running-log' }))
})

test('recenter stops verified services, clears registry state, and never kills pid aliases', async (t) => {
  rmSync(stateDir, { recursive: true, force: true })
  mkdirSync(stateDir, { recursive: true })
  const started = parseResult(
    await bridge.dispatch('service_start', {
      name: 'owned-service',
      command: `${process.execPath} -e "setInterval(() => {}, 1000)"`,
      cwd: probe,
      autostart: true,
    }),
  )
  assert.equal(started.ok, true)
  const unrelated = await realSleeper('unrelated-recenter-process')
  t.after(() => bridge.stopRegisteredProcess(unrelated.service))
  const activeShell = bridge.dispatch('shell', {
    command: `${process.execPath} -e "setInterval(() => {}, 1000)"`,
    cwd: probe,
    timeout_ms: 10_000,
  })
  t.after(() => bridge.recenterRegistry())
  for (let attempt = 0; attempt < 40; attempt++) {
    if (readdirSync(stateDir).some((entry) => entry.startsWith('.active-shell-'))) break
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
  assert.equal(readdirSync(stateDir).some((entry) => entry.startsWith('.active-shell-')), true)
  assert.equal(bridge.auditRegistry().summary.active_shells_verified, 1)
  const registry = JSON.parse(readFileSync(join(stateDir, 'registry.json'), 'utf8'))
  registry.alias = {
    ...unrelated.service,
    name: 'alias',
    process_identity: { ...unrelated.service.process_identity, start_ticks: '0' },
  }
  writeFileSync(join(stateDir, 'registry.json'), JSON.stringify(registry))

  const result = bridge.recenterRegistry()
  assert.equal(result.ok, true)
  assert.equal(result.processes_stopped, 1)
  assert.equal(result.shell_processes_stopped, 1)
  assert.equal(result.detached_pid_aliases, 1)
  assert.equal(bridge.inspectRegisteredProcess(registry['owned-service']).running, false)
  assert.equal(processExists(unrelated.child.pid), true)
  assert.deepEqual(JSON.parse(readFileSync(join(stateDir, 'registry.json'), 'utf8')), {})
  assert.equal(readdirSync(stateDir).some((entry) => entry.startsWith('.active-shell-')), false)
  assert.equal(bridge.auditRegistry().summary.active_shells_verified, 0)
  assert.equal((await activeShell).isError, true)
})
