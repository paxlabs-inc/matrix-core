// kindle bridge — token-metadata + image upload to the KindleLaunch media
// services, so a launched token actually SHOWS UP on the frontend.
//
// A successful on-chain createMarket only registers the pool; the consumer
// frontend renders a token from its media/metadata record (name, symbol,
// description, socials) and its logo/banner images. This module writes that
// record through the public media edge.
//
// AUTH (no human popup): the write is EIP-191 gated — media/metadata verifies
// VerifyWalletSignature(wallet, message, signature) and requires the wallet to
// be the pool CREATOR. The creator IS the per-user embedded wallet that signed
// createMarket, so we sign a one-time gasless message with that SAME wallet
// (wallet.signMessage / /v1/agent/sign-message). No second identity, no
// per-action user prompt — the agent signs server-side under its leash.
//
// WIRE (verified against media/gateway upload.go + media/metadata handlers.go):
//   POST {gateway}/upload/token/{addr}   (default; scans images, forwards) OR
//   POST {metadata}/metadata/{addr}      (direct to the authoritative writer)
//   multipart/form-data fields:
//     wallet    = creator 0x
//     message   = the signed challenge string
//     signature = EIP-191 signature of `message` by `wallet`
//     metadata  = JSON {name,symbol,description,website,twitter,telegram,discord,tags[],decimals?}
//     logo?     = image file (webp|png|svg|jpeg, <=2 MiB)
//     banner?   = image file (webp|png|svg|jpeg, <=5 MiB)
//   200 -> {success, metadata_updated, logo_url?, banner_url?, errors?}

import { MEDIA } from './config.mjs'

// Image MIME allowlist mirrors media/metadata image.AllowedMime.
const MIME_EXT = {
  'image/png': 'png',
  'image/webp': 'webp',
  'image/jpeg': 'jpg',
  'image/jpg': 'jpg',
  'image/svg+xml': 'svg',
}

function extForMime(mime) {
  return MIME_EXT[String(mime || '').toLowerCase()] ?? null
}

// resolveImage normalizes an image input into {bytes, contentType, filename} or
// null when none was provided. Accepts:
//   - a data: URI string ("data:image/png;base64,....")
//   - an http(s) URL string (fetched)
//   - an object { url } | { dataUri } | { base64, contentType }
// Rejects (throws) an unsupported MIME or an oversize image before any upload.
export async function resolveImage(input, kind, maxBytes) {
  if (input === undefined || input === null || input === '') return null

  let bytes
  let contentType

  if (typeof input === 'object' && !Array.isArray(input)) {
    if (input.base64) {
      bytes = Buffer.from(String(input.base64), 'base64')
      contentType = input.contentType || input.mime
    } else if (input.dataUri) {
      ;({ bytes, contentType } = decodeDataUri(input.dataUri))
    } else if (input.url) {
      ;({ bytes, contentType } = await fetchImage(input.url))
      contentType = input.contentType || contentType
    } else {
      throw new Error(`${kind}: provide a url, dataUri, or base64`)
    }
  } else {
    const s = String(input)
    if (s.startsWith('data:')) {
      ;({ bytes, contentType } = decodeDataUri(s))
    } else if (/^https?:\/\//i.test(s)) {
      ;({ bytes, contentType } = await fetchImage(s))
    } else {
      throw new Error(`${kind}: must be an http(s) URL or a data: URI`)
    }
  }

  if (!extForMime(contentType)) {
    throw new Error(`${kind}: unsupported image type ${contentType || '(unknown)'} — use png, webp, jpeg, or svg`)
  }
  if (maxBytes && bytes.length > maxBytes) {
    throw new Error(`${kind}: image is ${bytes.length} bytes, over the ${maxBytes}-byte limit`)
  }
  return { bytes, contentType: String(contentType).toLowerCase(), filename: `${kind}.${extForMime(contentType)}` }
}

function decodeDataUri(uri) {
  const m = /^data:([^;,]+)?(;base64)?,(.*)$/s.exec(String(uri))
  if (!m) throw new Error('invalid data: URI')
  const contentType = m[1] || 'application/octet-stream'
  const bytes = m[2] ? Buffer.from(m[3], 'base64') : Buffer.from(decodeURIComponent(m[3]), 'utf8')
  return { bytes, contentType }
}

async function fetchImage(url) {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), 20000)
  try {
    const res = await fetch(url, { signal: ctrl.signal, redirect: 'follow' })
    if (!res.ok) throw new Error(`fetch ${url} -> HTTP ${res.status}`)
    const contentType = res.headers.get('content-type')?.split(';')[0]?.trim()
    const bytes = Buffer.from(await res.arrayBuffer())
    return { bytes, contentType }
  } finally {
    clearTimeout(timer)
  }
}

// buildMetadataJSON keeps only the fields media/metadata's UploadMetadata
// understands, dropping empties so the upsert does not blank existing values.
export function buildMetadataJSON(meta = {}) {
  const out = {}
  for (const k of ['name', 'symbol', 'description', 'website', 'twitter', 'telegram', 'discord']) {
    const v = meta[k]
    if (v !== undefined && v !== null && String(v).trim() !== '') out[k] = String(v).trim()
  }
  if (Array.isArray(meta.tags) && meta.tags.length) out.tags = meta.tags.map((t) => String(t)).slice(0, 16)
  if (meta.decimals !== undefined && meta.decimals !== null && String(meta.decimals) !== '') {
    const d = Number(meta.decimals)
    if (Number.isInteger(d) && d >= 0 && d <= 36) out.decimals = d
  }
  return out
}

// signedChallenge renders the message the creator wallet signs. The content is
// free-form (media/metadata only checks the signature recovers to `wallet`);
// we make it human-meaningful and bound to the token + time.
export function signedChallenge(tokenAddress, wallet) {
  return (
    `KindleLaunch: set token metadata\n` +
    `Token: ${tokenAddress}\n` +
    `Wallet: ${wallet}\n` +
    `Issued At: ${new Date().toISOString()}`
  )
}

// uploadMetadata writes the token's metadata + optional images so it renders on
// the frontend. The creator wallet is resolved up front and `signMessage` is
// injected (wallet.signMessage) -> {signature,address}. Returns a structured
// result; never throws on a backend 4xx (returns {ok:false,...} so the launch
// flow can surface it without failing the launch).
export async function uploadMetadata({ tokenAddress, wallet, metadata, logo, banner, signMessage }) {
  const addr = String(tokenAddress || '').toLowerCase()
  if (!/^0x[0-9a-fA-F]{40}$/.test(addr)) throw new Error('uploadMetadata: invalid token address')
  if (typeof signMessage !== 'function') throw new Error('uploadMetadata: signMessage is required')

  const metaJSON = buildMetadataJSON(metadata)
  const logoImg = await resolveImage(logo, 'logo', MEDIA.maxLogoBytes)
  const bannerImg = await resolveImage(banner, 'banner', MEDIA.maxBannerBytes)
  if (Object.keys(metaJSON).length === 0 && !logoImg && !bannerImg) {
    throw new Error('uploadMetadata: nothing to upload (no metadata fields and no images)')
  }

  // Sign the self-describing challenge with the embedded wallet. media/metadata
  // recovers the signer and requires it to equal the pool creator.
  const message = signedChallenge(addr, String(wallet || '').toLowerCase())
  const sig = await signMessage(message)
  const signer = (sig && sig.address) || wallet
  const signature = sig && sig.signature
  if (!signer || !signature) throw new Error('uploadMetadata: wallet sign-message returned no signature')

  const form = new FormData()
  form.set('wallet', String(signer).toLowerCase())
  form.set('message', message)
  form.set('signature', signature)
  if (Object.keys(metaJSON).length) form.set('metadata', JSON.stringify(metaJSON))
  if (logoImg) form.set('logo', new Blob([logoImg.bytes], { type: logoImg.contentType }), logoImg.filename)
  if (bannerImg) form.set('banner', new Blob([bannerImg.bytes], { type: bannerImg.contentType }), bannerImg.filename)

  const url = MEDIA.uploadVia === 'metadata'
    ? `${MEDIA.metadata}/metadata/${addr}`
    : `${MEDIA.gateway}/upload/token/${addr}`

  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), 60000)
  let res
  let body
  try {
    res = await fetch(url, { method: 'POST', body: form, signal: ctrl.signal })
    body = await res.json().catch(() => null)
  } catch (e) {
    return { ok: false, uploaded: false, reason: `could not reach the media service (${e?.message ?? e})`, endpoint: url }
  } finally {
    clearTimeout(timer)
  }

  if (res.status === 404) {
    // media/metadata 404s until the indexer has seen the new pool (create-wizard
    // race). The launch itself succeeded; metadata can be set again shortly.
    return {
      ok: false, uploaded: false, indexer_pending: true, endpoint: url,
      reason: 'the pool is not indexed yet, so its metadata could not be written — retry kindle_set_metadata in a few seconds',
    }
  }
  if (res.status === 403) {
    return { ok: false, uploaded: false, endpoint: url, reason: 'the media service rejected the upload (not the pool creator or invalid signature)' }
  }
  if (!res.ok) {
    return { ok: false, uploaded: false, endpoint: url, status: res.status, reason: (body && (body.message || body.error)) || `media service returned HTTP ${res.status}` }
  }

  return {
    ok: body ? body.success !== false : true,
    uploaded: true,
    metadata_updated: body ? body.metadata_updated === true : undefined,
    logo_url: body ? body.logo_url ?? undefined : undefined,
    banner_url: body ? body.banner_url ?? undefined : undefined,
    errors: body && Array.isArray(body.errors) && body.errors.length ? body.errors : undefined,
    frontend: `${MEDIA.frontend}/token/${addr}`,
    metadata_json: metaJSON,
  }
}
