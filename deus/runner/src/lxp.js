/**
 * LXP (lxp/1) server middleware for Node services — the JS half of the one
 * protocol deus/pkg/lxp implements in Go: HTTP-native payments on LayerX.
 *
 * A priced request without X-LayerX-Payment answers 402 with lxp/1 terms
 * carrying a LayerX challenge nonce prefetched for the payer's DID; the retry
 * carries the payer-signed canonical intent, the middleware submits it to
 * layerxd (the signature IS the authorization), executes the handler, and
 * responds with X-LayerX-Receipt. Exact mode settles then serves; hold mode
 * reserves in the payer's own account, serves buffered, captures on 2xx and
 * releases on anything else. layerxd unreachable is 503 payment_unavailable —
 * never a free call. The service never custodies funds and never signs on a
 * payer's behalf.
 */

import { createPrivateKey, createPublicKey, sign as edSign, verify as edVerify } from 'node:crypto'

export const PROTOCOL = 'lxp/1'
export const HEADER_PAYMENT = 'x-layerx-payment'
export const HEADER_RECEIPT = 'X-LayerX-Receipt'
export const HEADER_CALLER_DID = 'x-caller-did'

export const REASONS = {
  paymentRequired: 'payment_required',
  identifyPayer: 'identify_payer',
  invalidPayment: 'invalid_payment',
  invalidSignature: 'invalid_signature',
  termsMismatch: 'terms_mismatch',
  paymentRejected: 'payment_rejected',
  insufficientFunds: 'insufficient_funds',
}

const DID_RE = /^did:matrix:[^:]+:([0-9a-fA-F]{16})$/
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex')
const ED25519_SPKI_PREFIX = Buffer.from('302a300506032b6570032100', 'hex')

export function parseUSDXMicro(s) {
  if (s == null || String(s).trim() === '') return null
  const m = /^([0-9]+)(?:\.([0-9]{1,6}))?$/.exec(String(s).trim())
  if (!m) return null
  return Number(m[1]) * 1_000_000 + Number((m[2] || '').padEnd(6, '0'))
}

export function formatUSDX(micro) {
  return `${Math.floor(micro / 1_000_000)}.${String(micro % 1_000_000).padStart(6, '0')}`
}

// Lockstep with layerxd auth.IntentMessage — byte-identical to deus/pkg/lxp.
export function intentMessage(op, did, nonce, ...fields) {
  return `matrix-layerx-intent:${op}:${did}:${fields.join(':')}:${nonce}`
}

export function payPreimage(p) {
  const fields = [p.to_did, p.amount_usdx]
  if (p.ref) fields.push(p.ref)
  return intentMessage('pay', p.from_did, p.nonce, ...fields)
}

export function holdPreimage(p, ttlSeconds, captorDid) {
  return intentMessage('hold', p.from_did, p.nonce, p.to_did, p.amount_usdx, String(ttlSeconds), p.ref || '', captorDid)
}

export function decodePaymentHeader(header) {
  try {
    return JSON.parse(Buffer.from(String(header).trim(), 'base64url').toString('utf8'))
  } catch {
    return null
  }
}

export function encodeReceiptHeader(r) {
  return Buffer.from(
    JSON.stringify({
      seq: r.seq,
      leaf_hash: r.leaf_hash,
      sequencer_sig: r.sequencer_sig,
      amount_usdx: r.amount_usdx,
      ...(r.ref ? { ref: r.ref } : {}),
    }),
  ).toString('base64url')
}

// verifyDIDSig mirrors layerxd's check: pubkey matches the DID fingerprint
// and signs the exact preimage bytes.
export function verifyDIDSig(did, pubHex, sigHex, preimage) {
  const m = DID_RE.exec(String(did).trim())
  if (!m) return 'malformed did'
  const pub = Buffer.from(String(pubHex).replace(/^0x/, ''), 'hex')
  if (pub.length !== 32) return 'invalid public key'
  if (pub.toString('hex').slice(0, 16) !== m[1].toLowerCase()) return 'public key does not match did fingerprint'
  const sig = Buffer.from(String(sigHex).replace(/^0x/, ''), 'hex')
  let ok = false
  try {
    const key = createPublicKey({ key: Buffer.concat([ED25519_SPKI_PREFIX, pub]), format: 'der', type: 'spki' })
    ok = edVerify(null, Buffer.from(preimage, 'utf8'), key, sig)
  } catch {
    ok = false
  }
  return ok ? null : 'signature verification failed'
}

class Unavailable extends Error {}

/**
 * createLXP wires one service's LXP half against one layerxd.
 * cfg: { layerxUrl, bearer?, keyHex?, didLabel? } — keyHex (32-byte ed25519
 * seed, hex) is required for hold mode (the service is the captor).
 */
export function createLXP(cfg) {
  const base = String(cfg.layerxUrl || '').replace(/\/+$/, '')
  if (!base) throw new Error('lxp: layerxUrl required')
  const bearer = cfg.bearer || ''

  let identity = null
  if (cfg.keyHex) {
    const seed = Buffer.from(String(cfg.keyHex).replace(/^0x/, ''), 'hex')
    if (seed.length !== 32) throw new Error('lxp: keyHex must be a 32-byte ed25519 seed (hex)')
    const privateKey = createPrivateKey({ key: Buffer.concat([ED25519_PKCS8_PREFIX, seed]), format: 'der', type: 'pkcs8' })
    const spki = createPublicKey(privateKey).export({ format: 'der', type: 'spki' })
    const pubHex = Buffer.from(spki.subarray(spki.length - 32)).toString('hex')
    identity = { did: `did:matrix:${cfg.didLabel || 'lxp-service'}:${pubHex.slice(0, 16)}`, pubHex, privateKey }
  }

  let agentToken = null
  let agentTokenExp = 0

  async function lx(method, path, body, token) {
    let res
    try {
      res = await fetch(base + path, {
        method,
        headers: {
          'Content-Type': 'application/json',
          ...(bearer ? { Authorization: `Bearer ${bearer}` } : {}),
          ...(token ? { 'X-LayerX-Agent': token } : {}),
        },
        body: body != null ? JSON.stringify(body) : undefined,
      })
    } catch (e) {
      throw new Unavailable(`layerx unreachable: ${e.message || e}`)
    }
    let env
    try {
      env = await res.json()
    } catch {
      if (res.status >= 500) throw new Unavailable(`layerx http ${res.status}`)
      throw new Error(`lxp: malformed layerx response (http ${res.status})`)
    }
    if (!env.ok) {
      const err = new Error(env.error?.message || `layerx error (http ${res.status})`)
      err.code = env.error?.code || (res.status >= 500 ? 'unavailable' : 'invalid_request')
      if (res.status >= 500 || res.status === 429) throw new Unavailable(err.message)
      throw err
    }
    return env.data
  }

  async function principalToken() {
    if (!identity) throw new Error('lxp: service key required for captor operations')
    if (agentToken && Date.now() < agentTokenExp) return agentToken
    const ch = await lx('POST', '/v1/agent/auth/challenge', { did: identity.did })
    const signature = edSign(null, Buffer.from(ch.message, 'utf8'), identity.privateKey).toString('hex')
    const vr = await lx('POST', '/v1/agent/auth/verify', {
      did: identity.did,
      public_key: identity.pubHex,
      nonce: ch.nonce,
      signature,
    })
    agentToken = vr.token
    agentTokenExp = Date.now() + (vr.expires_in - 30) * 1000
    return agentToken
  }

  /** challenge prefetches a nonce for payerDid and assembles lxp/1 terms. */
  async function challenge(payerDid, price) {
    const amount = parseUSDXMicro(price.amount_usdx)
    if (amount == null || amount <= 0) throw new Error(`lxp: price amount ${JSON.stringify(price.amount_usdx)} invalid`)
    const mode = price.mode || 'exact'
    if (mode !== 'exact' && mode !== 'hold') throw new Error(`lxp: unknown mode ${mode}`)
    const terms = {
      protocol: PROTOCOL,
      asset: 'USDX',
      amount_usdx: formatUSDX(amount),
      pay_to: price.pay_to,
      mode,
      layerx: base,
    }
    if (price.ref) terms.ref = price.ref
    if (price.quote_id) terms.quote_id = price.quote_id
    if (mode === 'hold') {
      if (!identity) throw new Error('lxp: hold mode requires a service key (captor identity)')
      terms.captor_did = identity.did
      terms.ttl_s = price.ttl_s > 0 ? price.ttl_s : 120
    }
    const ch = await lx('POST', '/v1/agent/auth/challenge', { did: payerDid })
    terms.nonce = ch.nonce
    terms.expires_at = new Date(Date.now() + ch.expires_in * 1000).toISOString()
    return terms
  }

  /** verifyAgainstTerms checks shape + signature locally BEFORE any submit. */
  function verifyAgainstTerms(p, price) {
    const amount = parseUSDXMicro(price.amount_usdx)
    if (amount == null) return REASONS.termsMismatch
    const mode = price.mode || 'exact'
    if (!p || !p.from_did || !p.public_key || !p.nonce || !p.signature) return REASONS.invalidPayment
    if (parseUSDXMicro(p.amount_usdx) !== amount || p.amount_usdx !== formatUSDX(amount)) return REASONS.termsMismatch
    if (p.to_did !== price.pay_to) return REASONS.termsMismatch
    if ((p.mode || 'exact') !== mode) return REASONS.termsMismatch
    if ((p.ref || '') !== (price.ref || '')) return REASONS.termsMismatch
    const preimage =
      mode === 'hold'
        ? holdPreimage(p, price.ttl_s > 0 ? price.ttl_s : 120, identity ? identity.did : '')
        : payPreimage(p)
    if (verifyDIDSig(p.from_did, p.public_key, p.signature, preimage) != null) return REASONS.invalidSignature
    return null
  }

  async function settleExact(p) {
    return lx('POST', '/v1/pay', {
      to_did: p.to_did,
      amount_usdx: p.amount_usdx,
      ref: p.ref || '',
      from_did: p.from_did,
      public_key: p.public_key,
      nonce: p.nonce,
      signature: p.signature,
    })
  }

  async function openHold(p, ttlSeconds) {
    return lx('POST', '/v1/hold', {
      to_did: p.to_did,
      amount_usdx: p.amount_usdx,
      captor_did: identity.did,
      ttl_s: ttlSeconds > 0 ? ttlSeconds : 120,
      ref: p.ref || '',
      from_did: p.from_did,
      public_key: p.public_key,
      nonce: p.nonce,
      signature: p.signature,
    })
  }

  async function capture(holdId, amountUSDX) {
    const token = await principalToken()
    const res = await lx('POST', `/v1/hold/${holdId}/capture`, { amount_usdx: amountUSDX }, token)
    return res.receipt
  }

  async function release(holdId) {
    const token = await principalToken()
    return lx('POST', `/v1/hold/${holdId}/release`, {}, token)
  }

  function settleReason(err) {
    if (err instanceof Unavailable) return { unavailable: true }
    if (err.code === 'insufficient_funds') return { reason: REASONS.insufficientFunds }
    if (err.code === 'unauthorized') return { reason: REASONS.paymentRejected }
    return { reason: REASONS.invalidPayment }
  }

  function writeJSON(res, status, body) {
    res.writeHead(status, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(body))
  }

  async function challengeOr503(res, payerDid, price, reason) {
    if (!DID_RE.test(payerDid || '')) {
      writeJSON(res, 402, { error: REASONS.paymentRequired, reason: REASONS.identifyPayer })
      return
    }
    let terms
    try {
      terms = await challenge(payerDid, price)
    } catch {
      writeJSON(res, 503, { error: 'payment_unavailable' })
      return
    }
    writeJSON(res, 402, { error: REASONS.paymentRequired, reason, lxp: terms })
  }

  /**
   * guard wraps a node:http handler behind the LXP paywall.
   * price: (req) => price object | null (null = free, passes through).
   */
  function guard(price, handler) {
    return async (req, res) => {
      let p
      try {
        p = await price(req)
      } catch {
        writeJSON(res, 500, { error: 'pricing_failed' })
        return
      }
      if (!p) {
        await handler(req, res)
        return
      }
      const mode = p.mode || 'exact'
      const header = req.headers[HEADER_PAYMENT]
      const payerHint = () => req.headers[HEADER_CALLER_DID] || ''
      if (!header) {
        await challengeOr503(res, payerHint(), p, REASONS.paymentRequired)
        return
      }
      const pay = decodePaymentHeader(header)
      if (!pay) {
        await challengeOr503(res, payerHint(), p, REASONS.invalidPayment)
        return
      }
      const reason = verifyAgainstTerms(pay, p)
      if (reason) {
        await challengeOr503(res, pay.from_did || payerHint(), p, reason)
        return
      }

      if (mode === 'exact') {
        let receipt
        try {
          receipt = await settleExact(pay)
        } catch (err) {
          const r = settleReason(err)
          if (r.unavailable) {
            writeJSON(res, 503, { error: 'payment_unavailable' })
            return
          }
          await challengeOr503(res, pay.from_did, p, r.reason)
          return
        }
        res.setHeader(HEADER_RECEIPT, encodeReceiptHeader(receipt))
        await handler(req, res)
        return
      }

      // hold: reserve -> execute buffered -> capture on 2xx / release otherwise
      let hold
      try {
        hold = await openHold(pay, p.ttl_s)
      } catch (err) {
        const r = settleReason(err)
        if (r.unavailable) {
          writeJSON(res, 503, { error: 'payment_unavailable' })
          return
        }
        await challengeOr503(res, pay.from_did, p, r.reason)
        return
      }
      const rec = bufferedResponse()
      try {
        await handler(req, rec)
      } catch (err) {
        await release(hold.hold_id).catch(() => {})
        writeJSON(res, 503, { error: 'execution_failed', message: String(err?.message || err) })
        return
      }
      if (rec.statusCode >= 200 && rec.statusCode < 300) {
        let receipt
        try {
          receipt = await capture(hold.hold_id, pay.amount_usdx)
        } catch {
          // Work is done but the capture failed — release so no funds strand.
          await release(hold.hold_id).catch(() => {})
          writeJSON(res, 503, { error: 'payment_unavailable' })
          return
        }
        rec.headers[HEADER_RECEIPT] = encodeReceiptHeader(receipt)
      } else {
        await release(hold.hold_id).catch(() => {})
      }
      rec.flush(res)
    }
  }

  return {
    did: () => (identity ? identity.did : ''),
    layerxUrl: () => base,
    challenge,
    verifyAgainstTerms,
    settleExact,
    openHold,
    capture,
    release,
    guard,
  }
}

// bufferedResponse captures a handler's full response so hold mode decides
// capture-vs-release before a byte reaches the client.
function bufferedResponse() {
  return {
    statusCode: 200,
    headers: {},
    chunks: [],
    setHeader(k, v) {
      this.headers[k] = v
    },
    getHeader(k) {
      return this.headers[k]
    },
    writeHead(status, hdrs) {
      this.statusCode = status
      Object.assign(this.headers, hdrs || {})
      return this
    },
    write(chunk) {
      this.chunks.push(Buffer.from(chunk))
      return true
    },
    end(chunk) {
      if (chunk != null) this.chunks.push(Buffer.from(chunk))
    },
    flush(res) {
      res.writeHead(this.statusCode, this.headers)
      res.end(Buffer.concat(this.chunks))
    },
  }
}
