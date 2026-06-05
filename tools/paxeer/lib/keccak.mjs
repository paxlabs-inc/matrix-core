// Keccak-256 — dependency-free, correct for Ethereum.
//
// IMPORTANT: This is Keccak (pad byte 0x01), NOT NIST SHA3-256 (pad 0x06).
// Node's crypto `sha3-256` is the NIST variant and produces DIFFERENT digests,
// which silently breaks function selectors, event topics, and address
// derivation. See the matrix `nodejs-keccak256` skill for the footgun. We
// implement Keccak-f[1600] with BigInt lanes — clarity + correctness over
// speed (an MCP bridge hashes tiny inputs, so BigInt is plenty fast).

const MASK64 = (1n << 64n) - 1n

const RC = [
  0x0000000000000001n, 0x0000000000008082n, 0x800000000000808an, 0x8000000080008000n,
  0x000000000000808bn, 0x0000000080000001n, 0x8000000080008081n, 0x8000000000008009n,
  0x000000000000008an, 0x0000000000000088n, 0x0000000080008009n, 0x000000008000000an,
  0x000000008000808bn, 0x800000000000008bn, 0x8000000000008089n, 0x8000000000008003n,
  0x8000000000008002n, 0x8000000000000080n, 0x000000000000800an, 0x800000008000000an,
  0x8000000080008081n, 0x8000000000008080n, 0x0000000080000001n, 0x8000000080008008n,
]

// Rotation offsets r[x][y]; flat lane index = x + 5*y.
const R = [
  [0n, 36n, 3n, 41n, 18n],
  [1n, 44n, 10n, 45n, 2n],
  [62n, 6n, 43n, 15n, 61n],
  [28n, 55n, 25n, 21n, 56n],
  [27n, 20n, 39n, 8n, 14n],
]

function rotl64(x, n) {
  n &= 63n
  if (n === 0n) return x & MASK64
  return ((x << n) | (x >> (64n - n))) & MASK64
}

function keccakF(A) {
  for (let round = 0; round < 24; round++) {
    // θ
    const C = new Array(5)
    for (let x = 0; x < 5; x++) {
      C[x] = A[x] ^ A[x + 5] ^ A[x + 10] ^ A[x + 15] ^ A[x + 20]
    }
    const D = new Array(5)
    for (let x = 0; x < 5; x++) {
      D[x] = C[(x + 4) % 5] ^ rotl64(C[(x + 1) % 5], 1n)
    }
    for (let x = 0; x < 5; x++) {
      for (let y = 0; y < 5; y++) A[x + 5 * y] = (A[x + 5 * y] ^ D[x]) & MASK64
    }
    // ρ + π
    const B = new Array(25).fill(0n)
    for (let x = 0; x < 5; x++) {
      for (let y = 0; y < 5; y++) {
        B[y + 5 * ((2 * x + 3 * y) % 5)] = rotl64(A[x + 5 * y], R[x][y])
      }
    }
    // χ
    for (let x = 0; x < 5; x++) {
      for (let y = 0; y < 5; y++) {
        A[x + 5 * y] = (B[x + 5 * y] ^ (~B[((x + 1) % 5) + 5 * y] & B[((x + 2) % 5) + 5 * y])) & MASK64
      }
    }
    // ι
    A[0] = (A[0] ^ RC[round]) & MASK64
  }
}

function loadLane(bytes, off) {
  let v = 0n
  for (let i = 7; i >= 0; i--) v = (v << 8n) | BigInt(bytes[off + i] | 0)
  return v
}

// keccak256 over raw bytes -> Uint8Array(32).
export function keccak256Bytes(input) {
  const msg = input instanceof Uint8Array ? input : new Uint8Array(input)
  const rate = 136 // 1088-bit rate for Keccak-256
  // pad10*1 with Keccak domain: append 0x01, zero-fill, final byte |= 0x80.
  const padLen = rate - (msg.length % rate)
  const padded = new Uint8Array(msg.length + padLen)
  padded.set(msg, 0)
  padded[msg.length] = 0x01
  padded[padded.length - 1] |= 0x80

  const A = new Array(25).fill(0n)
  for (let off = 0; off < padded.length; off += rate) {
    for (let i = 0; i < rate / 8; i++) {
      A[i] = (A[i] ^ loadLane(padded, off + i * 8)) & MASK64
    }
    keccakF(A)
  }

  const out = new Uint8Array(32)
  for (let i = 0; i < 4; i++) {
    let v = A[i]
    for (let b = 0; b < 8; b++) {
      out[i * 8 + b] = Number(v & 0xffn)
      v >>= 8n
    }
  }
  return out
}

export function toHex(bytes) {
  let s = '0x'
  for (const b of bytes) s += b.toString(16).padStart(2, '0')
  return s
}

function utf8Bytes(str) {
  return new Uint8Array(Buffer.from(str, 'utf8'))
}

export function hexToBytes(hex) {
  let h = String(hex).trim()
  if (h.startsWith('0x') || h.startsWith('0X')) h = h.slice(2)
  if (h.length % 2 !== 0) h = '0' + h
  const out = new Uint8Array(h.length / 2)
  for (let i = 0; i < out.length; i++) out[i] = parseInt(h.slice(i * 2, i * 2 + 2), 16)
  return out
}

// keccak256 of a UTF-8 string -> 0x-hex.
export function keccak256Utf8(str) {
  return toHex(keccak256Bytes(utf8Bytes(str)))
}

// keccak256 of a hex string -> 0x-hex.
export function keccak256Hex(hex) {
  return toHex(keccak256Bytes(hexToBytes(hex)))
}

// 4-byte function selector for a canonical signature like
// "transfer(address,uint256)" -> "0xa9059cbb".
export function selector(signature) {
  return toHex(keccak256Bytes(utf8Bytes(signature)).slice(0, 4))
}

// EIP-55 checksum address from a 20-byte hex address.
export function toChecksumAddress(addr) {
  const a = String(addr).toLowerCase().replace(/^0x/, '')
  const hash = toHex(keccak256Bytes(utf8Bytes(a))).slice(2)
  let out = '0x'
  for (let i = 0; i < a.length; i++) {
    out += parseInt(hash[i], 16) >= 8 ? a[i].toUpperCase() : a[i]
  }
  return out
}

// Self-test against known Keccak-256 vectors. Returns {ok, results}.
export function _selftest() {
  const cases = [
    { in: '', want: '0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470' },
    { in: 'abc', want: '0x4e03657aea45a94fc7d47ba826c8d667c0d1e6e33a64a036ec44f58fa12d6c45' },
    { sel: 'transfer(address,uint256)', want: '0xa9059cbb' },
    { sel: 'balanceOf(address)', want: '0x70a08231' },
    { sel: 'approve(address,uint256)', want: '0x095ea7b3' },
  ]
  const results = cases.map((c) => {
    const got = c.sel !== undefined ? selector(c.sel) : keccak256Utf8(c.in)
    return { ...c, got, pass: got === c.want }
  })
  return { ok: results.every((r) => r.pass), results }
}

// Run `node lib/keccak.mjs` to self-verify.
if (import.meta.url === `file://${process.argv[1]}`) {
  const t = _selftest()
  for (const r of t.results) {
    console.log(`${r.pass ? 'PASS' : 'FAIL'}  ${r.sel ?? JSON.stringify(r.in)} -> ${r.got}`)
  }
  console.log(t.ok ? 'keccak256 OK' : 'keccak256 FAILED')
  process.exit(t.ok ? 0 : 1)
}
