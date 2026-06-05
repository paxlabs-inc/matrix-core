// Minimal, dependency-free Ethereum ABI codec.
//
// Supports the types the paxeer precompiles + DEX contracts use: address,
// bool, uint<M>/int<M>, bytes<N>, bytes, string, dynamic arrays T[], fixed
// arrays T[k], and tuples (a,b,...). Enough to encode every write the bridge
// makes and decode every eth_call read it issues. Selectors come from the
// verified keccak256 in ./keccak.mjs.

import { selector, keccak256Bytes } from './keccak.mjs'

const MAX256 = (1n << 256n) - 1n

// ── helpers ────────────────────────────────────────────────────────────────
function strip0x(h) {
  const s = String(h).trim()
  return s.startsWith('0x') || s.startsWith('0X') ? s.slice(2) : s
}

function toBig(v) {
  if (typeof v === 'bigint') return v
  if (typeof v === 'number') {
    if (!Number.isInteger(v)) throw new Error(`abi: non-integer number ${v}`)
    return BigInt(v)
  }
  const s = String(v).trim()
  if (s === '') return 0n
  return s.startsWith('0x') || s.startsWith('0X') ? BigInt(s) : BigInt(s)
}

function word(b) {
  let x = toBig(b) & MAX256
  return x.toString(16).padStart(64, '0')
}

function rightPad(hex, multiple = 64) {
  const rem = hex.length % multiple
  return rem === 0 ? hex : hex + '0'.repeat(multiple - rem)
}

// ── type parsing ─────────────────────────────────────────────────────────
function splitTop(s) {
  const out = []
  let depth = 0
  let cur = ''
  for (const ch of s) {
    if (ch === '(' || ch === '[') depth++
    else if (ch === ')' || ch === ']') depth--
    if (ch === ',' && depth === 0) {
      out.push(cur)
      cur = ''
    } else cur += ch
  }
  if (cur.trim() !== '') out.push(cur)
  return out
}

function parseType(str) {
  const s = String(str).trim()
  const arr = s.match(/^(.*)\[(\d*)\]$/)
  if (arr) return { kind: 'array', base: parseType(arr[1]), fixed: arr[2] === '' ? null : Number(arr[2]) }
  if (s.startsWith('(') && s.endsWith(')')) {
    const inner = s.slice(1, -1)
    return { kind: 'tuple', comps: inner.trim() ? splitTop(inner).map(parseType) : [] }
  }
  return { kind: 'elem', type: s }
}

function isDynamic(n) {
  if (n.kind === 'elem') return n.type === 'bytes' || n.type === 'string'
  if (n.kind === 'array') return n.fixed === null ? true : isDynamic(n.base)
  return n.comps.some(isDynamic)
}

function staticWords(n) {
  if (n.kind === 'elem') return 1
  if (n.kind === 'array') return n.fixed * staticWords(n.base)
  return n.comps.reduce((a, c) => a + staticWords(c), 0)
}

// ── encoding ───────────────────────────────────────────────────────────────
function encodeElem(type, v) {
  if (type === 'address') {
    const a = strip0x(v).toLowerCase()
    if (a.length !== 40) throw new Error(`abi: bad address ${v}`)
    return a.padStart(64, '0')
  }
  if (type === 'bool') return word(v ? 1n : 0n)
  if (type.startsWith('uint')) return word(toBig(v))
  if (type.startsWith('int')) {
    let x = toBig(v)
    if (x < 0n) x = (1n << 256n) + x
    return word(x)
  }
  if (/^bytes([0-9]+)$/.test(type)) {
    const h = strip0x(v)
    if (h.length > 64) throw new Error(`abi: ${type} too long`)
    return rightPad(h, 64)
  }
  if (type === 'bytes') {
    const h = strip0x(v)
    const len = h.length / 2
    return word(BigInt(len)) + (h ? rightPad(h, 64) : '')
  }
  if (type === 'string') {
    const h = Buffer.from(String(v), 'utf8').toString('hex')
    const len = h.length / 2
    return word(BigInt(len)) + (h ? rightPad(h, 64) : '')
  }
  throw new Error(`abi: unsupported type ${type}`)
}

function encodeValue(n, v) {
  if (n.kind === 'elem') return encodeElem(n.type, v)
  if (n.kind === 'array') {
    const items = v || []
    if (n.fixed !== null && items.length !== n.fixed) throw new Error('abi: fixed array length mismatch')
    const body = encodeTuple(items.map(() => n.base), items)
    return n.fixed === null ? word(BigInt(items.length)) + body : body
  }
  return encodeTuple(n.comps, v || [])
}

function encodeTuple(nodes, values) {
  let headWords = 0
  for (const n of nodes) headWords += isDynamic(n) ? 1 : staticWords(n)
  let head = ''
  let tail = ''
  let tailOffset = headWords * 32
  for (let i = 0; i < nodes.length; i++) {
    const n = nodes[i]
    if (isDynamic(n)) {
      head += word(BigInt(tailOffset))
      const enc = encodeValue(n, values[i])
      tail += enc
      tailOffset += enc.length / 2
    } else {
      head += encodeValue(n, values[i])
    }
  }
  return head + tail
}

// encode(types[], values[]) -> hex (no 0x, no selector).
export function encode(types, values) {
  const nodes = types.map(parseType)
  return encodeTuple(nodes, values)
}

// encodeCall("name(t1,t2)", values[]) -> "0x" + selector + encoded args.
export function encodeCall(signature, values = []) {
  const m = signature.match(/^([^(]+)\((.*)\)$/)
  if (!m) throw new Error(`abi: bad signature ${signature}`)
  const types = m[2].trim() ? splitTop(m[2]) : []
  return selector(signature) + encode(types, values).replace(/^/, '')
}

// ── decoding ───────────────────────────────────────────────────────────────
function wordAt(buf, byteOffset) {
  return buf.slice(byteOffset * 2, byteOffset * 2 + 64)
}

function decodeElem(type, buf, at) {
  const w = wordAt(buf, at)
  if (type === 'address') return '0x' + w.slice(24)
  if (type === 'bool') return BigInt('0x' + w) !== 0n
  if (type.startsWith('uint')) return BigInt('0x' + w).toString()
  if (type.startsWith('int')) {
    const bits = Number(type.slice(3) || '256')
    let x = BigInt('0x' + w)
    if (x >> BigInt(bits - 1) & 1n) x -= 1n << BigInt(bits)
    return x.toString()
  }
  if (/^bytes([0-9]+)$/.test(type)) {
    const n = Number(type.match(/^bytes([0-9]+)$/)[1])
    return '0x' + w.slice(0, n * 2)
  }
  if (type === 'bytes' || type === 'string') {
    const len = Number(BigInt('0x' + wordAt(buf, at)))
    const dataHex = buf.slice((at + 32) * 2, (at + 32) * 2 + len * 2)
    return type === 'string' ? Buffer.from(dataHex, 'hex').toString('utf8') : '0x' + dataHex
  }
  throw new Error(`abi: unsupported decode type ${type}`)
}

function decodeTuple(nodes, buf, base) {
  let head = base
  const out = []
  for (const n of nodes) {
    if (isDynamic(n)) {
      const off = Number(BigInt('0x' + wordAt(buf, head)))
      out.push(decodeValue(n, buf, base + off))
      head += 32
    } else {
      out.push(decodeValue(n, buf, head))
      head += staticWords(n) * 32
    }
  }
  return out
}

function decodeValue(n, buf, at) {
  if (n.kind === 'elem') return decodeElem(n.type, buf, at)
  if (n.kind === 'array') {
    if (n.fixed === null) {
      const len = Number(BigInt('0x' + wordAt(buf, at)))
      return decodeTuple(Array(len).fill(n.base), buf, at + 32)
    }
    return decodeTuple(Array(n.fixed).fill(n.base), buf, at)
  }
  return decodeTuple(n.comps, buf, at)
}

// decode(types[], hex) -> values[].
export function decode(types, data) {
  return decodeTuple(types.map(parseType), strip0x(data), 0)
}

// Self-test: encode/decode round-trips against known vectors.
export function _selftest() {
  const results = []
  const T = (name, got, want) => results.push({ name, got, want, pass: got === want })

  const call = encodeCall('transfer(address,uint256)', ['0x0000000000000000000000000000000000000001', '1'])
  T('encodeCall transfer', call,
    '0xa9059cbb' +
    '0000000000000000000000000000000000000000000000000000000000000001' +
    '0000000000000000000000000000000000000000000000000000000000000001')

  T('decode uint256', decode(['uint256'], '0x' + '0'.repeat(63) + 'a')[0], '10')
  T('decode address', decode(['address'], '0x' + '0'.repeat(24) + 'de0b295669a9fd93d5f28d9ec85e40f4cb697bae')[0],
    '0xde0b295669a9fd93d5f28d9ec85e40f4cb697bae')

  const enc = encode(['address[]'], [['0x0000000000000000000000000000000000000001', '0x0000000000000000000000000000000000000002']])
  const dec = decode(['address[]'], '0x' + enc)[0]
  T('roundtrip address[]', JSON.stringify(dec),
    JSON.stringify(['0x0000000000000000000000000000000000000001', '0x0000000000000000000000000000000000000002']))

  const b = encode(['bytes'], ['0xdeadbeef'])
  T('roundtrip bytes', decode(['bytes'], '0x' + b)[0], '0xdeadbeef')

  // static tuple (uint256,address,bool) inline
  const tup = encode(['(uint256,address,bool)'], [['7', '0x0000000000000000000000000000000000000009', true]])
  const tdec = decode(['(uint256,address,bool)'], '0x' + tup)[0]
  T('roundtrip static tuple', JSON.stringify(tdec),
    JSON.stringify(['7', '0x0000000000000000000000000000000000000009', true]))

  return { ok: results.every((r) => r.pass), results }
}

// Keccak helper re-export for callers that build topics/ids.
export { keccak256Bytes }

if (import.meta.url === `file://${process.argv[1]}`) {
  const t = _selftest()
  for (const r of t.results) console.log(`${r.pass ? 'PASS' : 'FAIL'}  ${r.name}`)
  if (!t.ok) for (const r of t.results.filter((x) => !x.pass)) console.log('  got ', r.got, '\n  want', r.want)
  console.log(t.ok ? 'abi OK' : 'abi FAILED')
  process.exit(t.ok ? 0 : 1)
}
