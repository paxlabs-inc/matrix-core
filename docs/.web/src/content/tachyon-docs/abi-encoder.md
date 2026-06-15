# ABI Encoder

**Source file:** `internal/abienc/abienc.go`

The ABI encoder bridges JSON-decoded values to the exact Go types required by go-ethereum's ABI packer. JSON decoding produces `float64/string/bool/[]interface{}/map[string]interface{}`; the ABI packer needs `*big.Int` for `uint256`, `common.Address` for addresses, `[N]byte` for fixed bytes, etc. This package coerces each value into the precise Go type the ABI expects.

---

## Design decisions

### JSON-to-Go type coercion, not schema validation

The encoder does not validate that the JSON matches the ABI schema. It coerces each value into the Go type the ABI expects. If coercion fails, it returns a detailed error (`arg %d (%s): %w`). This is sufficient for agent use because agents typically construct well-formed JSON args.

### Integer handling

`uint256` and other large integers are represented as strings in JSON (decimal or `0x` hex) to avoid JavaScript's `2^53` precision limit. The encoder parses these strings into `*big.Int`:

```go
func toBigInt(v interface{}) (*big.Int, error)
```

Supported input types:
- `*big.Int` — passthrough
- `json.Number` — decimal string
- `string` — decimal or `0x` hex
- `float64` — converted via `big.NewFloat` (rejects non-integral)

For ABI types smaller than 256 bits, the encoder returns the exact Go type (`int8/16/32/64`, `uint8/16/32/64`) to satisfy go-ethereum's packer.

### Tuple support

Tuples accept either:
- An ordered JSON array (positional)
- A JSON object keyed by Solidity field names (named)

```go
// Array form:
["0xRecipient", "1000000000000000000"]

// Object form:
{"to": "0xRecipient", "amount": "1000000000000000000"}
```

The encoder uses `abi.Type.TupleRawNames` to map object keys to struct fields.

### Array/slice support

Dynamic arrays (`uint256[]`) and fixed arrays (`uint256[3]`) are both supported. The encoder builds the correct Go slice or array type via reflection.

### Bytes support

- `bytes` (dynamic): hex string → `[]byte`
- `bytesN` (fixed): hex string → `[N]byte` via reflection

---

## API

```go
// Pack a method call (4-byte selector + arguments)
func Pack(abiJSON []byte, method string, args any) ([]byte, error)

// Pack constructor arguments (no selector)
func PackConstructorArgs(abiJSON []byte, args any) ([]byte, error)
```

---

## Coercion matrix

| ABI type | JSON input | Go output |
|---|---|---|
| `int8`–`int256` | number/string | `int8/16/32/64` or `*big.Int` |
| `uint8`–`uint256` | number/string | `uint8/16/32/64` or `*big.Int` |
| `bool` | boolean | `bool` |
| `string` | string | `string` |
| `address` | hex string | `common.Address` |
| `bytes` | hex string | `[]byte` |
| `bytesN` | hex string | `[N]byte` |
| `T[]` | array | `[]T` |
| `T[N]` | array | `[N]T` |
| `tuple` | array or object | struct |

---

## Modifying the ABI encoder

| What to change | Where |
|---|---|
| Add new ABI type | `internal/abienc/abienc.go` — `coerce` switch |
| Change integer parsing | `internal/abienc/abienc.go` — `toBigInt` |
| Add tuple field alias | `internal/abienc/abienc.go` — `coerceTuple` |
| Add struct tag support | `internal/abienc/abienc.go` — use `reflect` tags |
