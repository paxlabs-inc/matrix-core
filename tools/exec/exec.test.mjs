import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test, { after } from 'node:test'

const probe = mkdtempSync(join(tmpdir(), 'matrix-exec-identity-'))
const stateDir = join(probe, 'services')
mkdirSync(stateDir, { recursive: true })
process.env.MATRIX_EXEC_STATE_DIR = stateDir
process.env.MATRIX_EXEC_WORKDIR = probe

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
