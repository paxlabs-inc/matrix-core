import test from 'node:test'
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { mkdtemp, readFile, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createInterface } from 'node:readline'

const bridge = new URL('./media.mjs', import.meta.url)

async function callTool(name, args, env = {}) {
  const child = spawn(process.execPath, [bridge.pathname], {
    env: { ...process.env, XAI_API_KEY: '', NOVITA_API_KEY: '', MIMO_API_KEY: 'validation-only', ...env },
    stdio: ['pipe', 'pipe', 'pipe'],
  })
  const stderr = []
  child.stderr.on('data', (chunk) => stderr.push(chunk))
  const lines = createInterface({ input: child.stdout })
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }) + '\n')
  const response = await new Promise((resolve, reject) => {
    lines.once('line', (line) => resolve(JSON.parse(line)))
    child.once('error', reject)
    child.once('exit', (code) => {
      if (code && code !== 0) reject(new Error(`bridge exited ${code}: ${Buffer.concat(stderr).toString()}`))
    })
  })
  child.kill()
  return response.result
}

function payload(result) {
  assert.equal(result.content.length, 1)
  return JSON.parse(result.content[0].text)
}

test('registry and implementations are bijective across shipped manifests', async () => {
  const child = spawn(process.execPath, [bridge.pathname, '--selftest'], { stdio: ['ignore', 'pipe', 'pipe'] })
  const stdout = []
  const stderr = []
  child.stdout.on('data', (chunk) => stdout.push(chunk))
  child.stderr.on('data', (chunk) => stderr.push(chunk))
  const code = await new Promise((resolve, reject) => {
    child.once('error', reject)
    child.once('exit', resolve)
  })
  assert.equal(code, 0, Buffer.concat(stderr).toString())
  const text = Buffer.concat(stdout).toString()
  assert.match(text, /transcribe_audio/)
  assert.match(text, /media OK \(2 manifests verified\)/)
})

test('transcribe_audio rejects an unknown language before provider dispatch', async () => {
  const result = await callTool('transcribe_audio', { audio: '/media/speech.wav', language: 'klingon' })
  assert.equal(result.isError, true)
  assert.match(payload(result).error, /auto.*zh.*en/)
})

test('transcribe_audio rejects an input whose encoded form exceeds 10 MB', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'matrix-media-asr-'))
  await writeFile(join(dir, 'large.wav'), Buffer.alloc(7_864_321))
  const result = await callTool('transcribe_audio', { audio: '/media/large.wav' }, { MATRIX_MEDIA_DIR: dir, MATRIX_MEDIA_BASE: '/media' })
  assert.equal(result.isError, true)
  assert.match(payload(result).error, /10 MB/)
})

test('live MiMo ASR transcribes a real wav fixture', { skip: !process.env.MIMO_API_KEY || !process.env.MIMO_ASR_TEST_AUDIO }, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'matrix-media-asr-live-'))
  const wav = await readFile(process.env.MIMO_ASR_TEST_AUDIO)
  await writeFile(join(dir, 'speech.wav'), wav)
  const result = await callTool('transcribe_audio', { audio: '/media/speech.wav', language: 'en' }, {
    MIMO_API_KEY: process.env.MIMO_API_KEY,
    MATRIX_MEDIA_DIR: dir,
    MATRIX_MEDIA_BASE: '/media',
  })
  assert.equal(result.isError, false, JSON.stringify(payload(result)))
  assert.ok(payload(result).transcript.length > 0)
})

test('live MiMo TTS honors voice and style and persists a wav', { skip: !process.env.MIMO_API_KEY }, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'matrix-media-tts-live-'))
  const style = 'Speak calmly with a measured pace and a warm tone.'
  const result = await callTool('generate_speech', { text: 'The voice path is working.', voice: 'Mia', style }, {
    MIMO_API_KEY: process.env.MIMO_API_KEY,
    MATRIX_MEDIA_DIR: dir,
    MATRIX_MEDIA_BASE: '/media',
  })
  assert.equal(result.isError, false, JSON.stringify(payload(result)))
  const out = payload(result)
  assert.equal(out.voice, 'Mia')
  assert.equal(out.style, style)
  assert.equal(out.mime, 'audio/wav')
  const wav = await readFile(join(dir, out.url.split('/').pop()))
  assert.equal(wav.subarray(0, 4).toString('ascii'), 'RIFF')
})
