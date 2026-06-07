package abienc

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// erc20ABI is a minimal ERC-20 fragment exercising address + uint256 + bool.
const erc20ABI = `[
  {"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
  {"type":"function","name":"approve","inputs":[{"name":"spender","type":"address"},{"name":"value","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
  {"type":"function","name":"setFlags","inputs":[{"name":"a","type":"bool"},{"name":"label","type":"string"},{"name":"payload","type":"bytes"}],"outputs":[]},
  {"type":"function","name":"setRoot","inputs":[{"name":"root","type":"bytes32"}],"outputs":[]},
  {"type":"function","name":"setTier","inputs":[{"name":"t","type":"uint8"}],"outputs":[]},
  {"type":"function","name":"batch","inputs":[{"name":"tos","type":"address[]"},{"name":"amts","type":"uint256[]"}],"outputs":[]},
  {"type":"function","name":"order","inputs":[{"name":"o","type":"tuple","components":[{"name":"trader","type":"address"},{"name":"size","type":"uint256"},{"name":"long","type":"bool"}]}],"outputs":[]}
]`

func parse(t *testing.T) abi.ABI {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		t.Fatalf("parse abi: %v", err)
	}
	return parsed
}

// unpack decodes calldata (selector + args) back into Go values for assertions.
func unpack(t *testing.T, parsed abi.ABI, method string, data []byte) []interface{} {
	t.Helper()
	m, ok := parsed.Methods[method]
	if !ok {
		t.Fatalf("method %q not in abi", method)
	}
	if len(data) < 4 {
		t.Fatalf("calldata too short: %d bytes", len(data))
	}
	vals, err := m.Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("unpack %s: %v", method, err)
	}
	return vals
}

func TestPackAddressUint256(t *testing.T) {
	parsed := parse(t)
	addr := "0x1111111111111111111111111111111111111111"
	data, err := Pack([]byte(erc20ABI), "transfer", []interface{}{addr, "1000000000000000000"})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	// Selector must match the canonical transfer(address,uint256).
	want := parsed.Methods["transfer"].ID
	if string(data[:4]) != string(want) {
		t.Fatalf("selector mismatch: got %x want %x", data[:4], want)
	}
	vals := unpack(t, parsed, "transfer", data)
	if got := vals[0].(common.Address); got != common.HexToAddress(addr) {
		t.Fatalf("address mismatch: %s", got.Hex())
	}
	if got := vals[1].(*big.Int); got.String() != "1000000000000000000" {
		t.Fatalf("amount mismatch: %s", got.String())
	}
}

func TestPackHexUint(t *testing.T) {
	parsed := parse(t)
	data, err := Pack([]byte(erc20ABI), "approve",
		[]interface{}{"0x2222222222222222222222222222222222222222", "0xff"})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	vals := unpack(t, parsed, "approve", data)
	if got := vals[1].(*big.Int); got.Int64() != 255 {
		t.Fatalf("hex uint mismatch: %s", got.String())
	}
}

func TestPackBoolStringBytes(t *testing.T) {
	parsed := parse(t)
	data, err := Pack([]byte(erc20ABI), "setFlags",
		[]interface{}{true, "hello", "0xdeadbeef"})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	vals := unpack(t, parsed, "setFlags", data)
	if vals[0].(bool) != true {
		t.Fatalf("bool mismatch")
	}
	if vals[1].(string) != "hello" {
		t.Fatalf("string mismatch: %v", vals[1])
	}
	if got := vals[2].([]byte); len(got) != 4 || got[0] != 0xde {
		t.Fatalf("bytes mismatch: %x", got)
	}
}

func TestPackFixedBytes32(t *testing.T) {
	parsed := parse(t)
	root := "0x" + strings.Repeat("ab", 32)
	data, err := Pack([]byte(erc20ABI), "setRoot", []interface{}{root})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	vals := unpack(t, parsed, "setRoot", data)
	got := vals[0].([32]byte)
	if got[0] != 0xab || got[31] != 0xab {
		t.Fatalf("bytes32 mismatch: %x", got)
	}
}

func TestPackUint8(t *testing.T) {
	parsed := parse(t)
	data, err := Pack([]byte(erc20ABI), "setTier", []interface{}{float64(7)})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	vals := unpack(t, parsed, "setTier", data)
	if got := vals[0].(uint8); got != 7 {
		t.Fatalf("uint8 mismatch: %d", got)
	}
}

func TestPackArrays(t *testing.T) {
	parsed := parse(t)
	tos := []interface{}{
		"0x1111111111111111111111111111111111111111",
		"0x2222222222222222222222222222222222222222",
	}
	amts := []interface{}{"10", "20"}
	data, err := Pack([]byte(erc20ABI), "batch", []interface{}{tos, amts})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	vals := unpack(t, parsed, "batch", data)
	addrs := vals[0].([]common.Address)
	if len(addrs) != 2 || addrs[1] != common.HexToAddress(tos[1].(string)) {
		t.Fatalf("address array mismatch: %v", addrs)
	}
	nums := vals[1].([]*big.Int)
	if len(nums) != 2 || nums[1].Int64() != 20 {
		t.Fatalf("uint array mismatch: %v", nums)
	}
}

func TestPackTupleByArray(t *testing.T) {
	data, err := Pack([]byte(erc20ABI), "order", []interface{}{
		[]interface{}{"0x3333333333333333333333333333333333333333", "42", true},
	})
	if err != nil {
		t.Fatalf("pack tuple array: %v", err)
	}
	if len(data) < 4 {
		t.Fatalf("short calldata")
	}
}

func TestPackTupleByObject(t *testing.T) {
	var args []interface{}
	if err := json.Unmarshal([]byte(`[{"trader":"0x3333333333333333333333333333333333333333","size":"42","long":true}]`), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, err := Pack([]byte(erc20ABI), "order", args)
	if err != nil {
		t.Fatalf("pack tuple object: %v", err)
	}
	if len(data) < 4 {
		t.Fatalf("short calldata")
	}
}

func TestPackArgCountMismatch(t *testing.T) {
	_, err := Pack([]byte(erc20ABI), "transfer", []interface{}{"0x1111111111111111111111111111111111111111"})
	if err == nil {
		t.Fatalf("expected arg count error")
	}
}

func TestPackInvalidAddress(t *testing.T) {
	_, err := Pack([]byte(erc20ABI), "transfer", []interface{}{"not-an-address", "1"})
	if err == nil {
		t.Fatalf("expected invalid address error")
	}
}

func TestPackUnknownMethod(t *testing.T) {
	_, err := Pack([]byte(erc20ABI), "nope", []interface{}{})
	if err == nil {
		t.Fatalf("expected unknown method error")
	}
}

func TestPackRawMessageArgs(t *testing.T) {
	parsed := parse(t)
	raw := json.RawMessage(`["0x4444444444444444444444444444444444444444","5"]`)
	data, err := Pack([]byte(erc20ABI), "transfer", raw)
	if err != nil {
		t.Fatalf("pack raw: %v", err)
	}
	vals := unpack(t, parsed, "transfer", data)
	if got := vals[1].(*big.Int); got.Int64() != 5 {
		t.Fatalf("raw args amount mismatch: %s", got.String())
	}
}

const ctorABI = `[{"type":"constructor","inputs":[{"name":"owner","type":"address"},{"name":"cap","type":"uint256"}]}]`

func TestPackConstructorArgs(t *testing.T) {
	packed, err := PackConstructorArgs([]byte(ctorABI),
		json.RawMessage(`["0x5555555555555555555555555555555555555555","1000"]`))
	if err != nil {
		t.Fatalf("pack constructor: %v", err)
	}
	// address (32) + uint256 (32) = 64 bytes, no selector.
	if len(packed) != 64 {
		t.Fatalf("constructor arg length: got %d want 64", len(packed))
	}
}

func TestPackConstructorNoArgs(t *testing.T) {
	packed, err := PackConstructorArgs([]byte(`[{"type":"constructor","inputs":[]}]`), nil)
	if err != nil {
		t.Fatalf("pack empty constructor: %v", err)
	}
	if len(packed) != 0 {
		t.Fatalf("expected empty packing, got %d bytes", len(packed))
	}
}
