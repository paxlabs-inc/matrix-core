import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createPrivateKey, createPublicKey, verify as edVerify } from 'node:crypto'

import {
  parseUSDXMicro,
  formatUSDX,
  intentMessage,
  payPreimage,
  holdPreimage,
  signPayment,
  encodePaymentHeader,
  decodeReceiptHeader,
  readSpendJournal,
  recordSpend,
  leashCheck,
} from './deus.mjs'

const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')

function testIdentity() {
  const seed = Buffer.alloc(32, 7)
  const privateKey = createPrivateKey({ key: Buffer.concat([ED25519_PKCS8_PREFIX, seed]), format: 'der', type: 'pkcs8' })
  const spki = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
  const pubHex = Buffer.from(spki.subarray(spki.length - 32)).toString('hex')
  return { did: `did:matrix:t:${pubHex.slice(0, 16)}`, pubHex, privateKey }
}

test('usdx parse/format round trip', () => {
  assert.equal(parseUSDXMicro('0.031500'), 31500)
  assert.equal(parseUSDXMicro('2'), 2_000_000)
  assert.equal(parseUSDXMicro('0.5'), 500_000)
  assert.equal(parseUSDXMicro(''), null)
  assert.equal(parseUSDXMicro('1.2345678'), null)
  assert.equal(parseUSDXMicro('-1'), null)
  assert.equal(formatUSDX(31500), '0.031500')
  assert.equal(formatUSDX(2_000_000), '2.000000')
})

test('preimages are lockstep with layerxd auth.IntentMessage', () => {
  assert.equal(intentMessage('pay', 'did:matrix:a:b', 'n1', 'x', 'y'), 'matrix-layerx-intent:pay:did:matrix:a:b:x:y:n1')
  const base = { from_did: 'did:matrix:a:0011223344556677', nonce: 'n-1', to_did: 'did:matrix:p:8899aabbccddeeff', amount_usdx: '0.031500' }
  // pay: ref joins the signed fields only when present (ref-less preimage unchanged)
  assert.equal(payPreimage(base), 'matrix-layerx-intent:pay:did:matrix:a:0011223344556677:did:matrix:p:8899aabbccddeeff:0.031500:n-1')
  assert.equal(
    payPreimage({ ...base, ref: '0xabc' }),
    'matrix-layerx-intent:pay:did:matrix:a:0011223344556677:did:matrix:p:8899aabbccddeeff:0.031500:0xabc:n-1',
  )
  // hold: payee, amount, ttl, ref (may be empty), captor — always all fields
  assert.equal(
    holdPreimage({ ...base, ref: '0xabc' }, 60, 'did:matrix:g:ffeeddccbbaa9988'),
    'matrix-layerx-intent:hold:did:matrix:a:0011223344556677:did:matrix:p:8899aabbccddeeff:0.031500:60:0xabc:did:matrix:g:ffeeddccbbaa9988:n-1',
  )
  assert.equal(
    holdPreimage(base, 120, 'did:matrix:g:ffeeddccbbaa9988'),
    'matrix-layerx-intent:hold:did:matrix:a:0011223344556677:did:matrix:p:8899aabbccddeeff:0.031500:120::did:matrix:g:ffeeddccbbaa9988:n-1',
  )
})

test('signPayment signs the canonical preimage with the executor key', () => {
  const id = testIdentity()
  const terms = { pay_to: 'did:matrix:p:8899aabbccddeeff', amount_usdx: '0.031500', mode: 'hold', ttl_s: 60, captor_did: 'did:matrix:g:ffeeddccbbaa9988', nonce: 'n-2', ref: '0xdef' }
  const p = signPayment(terms, id)
  assert.equal(p.from_did, id.did)
  assert.equal(p.mode, 'hold')
  const preimage = holdPreimage(p, 60, terms.captor_did)
  const pub = createPublicKey({
    key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(id.pubHex, 'hex')]),
    format: 'der',
    type: 'spki',
  })
  assert.ok(edVerify(null, Buffer.from(preimage, 'utf8'), pub, Buffer.from(p.signature, 'hex')))
  // header round-trips
  const decoded = JSON.parse(Buffer.from(encodePaymentHeader(p), 'base64url').toString('utf8'))
  assert.deepEqual(decoded, p)
})

test('receipt header decode', () => {
  const r = { seq: 42, leaf_hash: '0xleaf', sequencer_sig: '0xsig', amount_usdx: '0.031500', ref: '0xr' }
  const h = Buffer.from(JSON.stringify(r)).toString('base64url')
  assert.deepEqual(decodeReceiptHeader(h), r)
  assert.equal(decodeReceiptHeader('%%%'), null)
  assert.equal(decodeReceiptHeader(''), null)
})

test('leash: no per-call cap means no invisible payments', () => {
  const res = leashCheck(31500, { maxSpendMicro: null, maxDailyMicro: null, journalPath: '/nonexistent' })
  assert.equal(res.ok, false)
  assert.equal(res.reason, 'auto_payment_disabled')
})

test('leash: per-call and rolling daily bounds', () => {
  const dir = mkdtempSync(join(tmpdir(), 'lxp-leash-'))
  const journal = join(dir, 'spend.json')
  const leash = { maxSpendMicro: 50_000, maxDailyMicro: 100_000, journalPath: journal }

  assert.equal(leashCheck(60_000, leash).reason, 'over_per_call_leash')
  assert.equal(leashCheck(31_500, leash).ok, true)

  const now = Date.now()
  recordSpend(31_500, journal, now)
  recordSpend(31_500, journal, now)
  assert.equal(readSpendJournal(journal, now).length, 2)
  // 63_000 spent; +40_000 would cross the 100_000 daily cap
  assert.equal(leashCheck(40_000, leash, now).reason, 'over_daily_leash')
  assert.equal(leashCheck(30_000, leash, now).ok, true)

  // entries older than the rolling 24h window fall out
  const later = now + 25 * 3600 * 1000
  assert.equal(readSpendJournal(journal, later).length, 0)
  assert.equal(leashCheck(40_000, leash, later).ok, true)

  // journal survives on disk (daemon-side durability across restarts)
  const raw = JSON.parse(readFileSync(journal, 'utf8'))
  assert.equal(raw.entries.length, 2)
})
