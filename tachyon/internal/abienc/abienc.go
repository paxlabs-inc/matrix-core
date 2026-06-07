// Package abienc ABI-encodes contract calls and constructor arguments from
// JSON-decoded values. The go-ethereum abi packer requires exact Go types
// (e.g. *big.Int for uint256, common.Address for address); JSON decoding only
// produces float64/string/bool/[]interface{}/map. This package bridges the gap
// by coercing each decoded value into the precise Go type the ABI expects,
// driven by the contract ABI itself.
package abienc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Pack ABI-encodes a method invocation (4-byte selector + arguments) using the
// contract ABI JSON. args may be nil, a []interface{}, a json.RawMessage array,
// or any value that marshals to a JSON array.
func Pack(abiJSON []byte, method string, args any) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		return nil, fmt.Errorf("parse abi: %w", err)
	}
	m, ok := parsed.Methods[method]
	if !ok {
		return nil, fmt.Errorf("method %q not found in abi", method)
	}
	coerced, err := coerceArgs(m.Inputs, args)
	if err != nil {
		return nil, err
	}
	return parsed.Pack(method, coerced...)
}

// PackConstructorArgs ABI-encodes constructor arguments (no selector) for
// appending to creation bytecode.
func PackConstructorArgs(abiJSON []byte, args any) ([]byte, error) {
	parsed, err := abi.JSON(strings.NewReader(string(abiJSON)))
	if err != nil {
		return nil, fmt.Errorf("parse abi: %w", err)
	}
	coerced, err := coerceArgs(parsed.Constructor.Inputs, args)
	if err != nil {
		return nil, err
	}
	return parsed.Constructor.Inputs.Pack(coerced...)
}

// coerceArgs converts a list of JSON-decoded values into the exact Go types
// required by the given ABI arguments.
func coerceArgs(arguments abi.Arguments, args any) ([]interface{}, error) {
	list, err := toList(args)
	if err != nil {
		return nil, err
	}
	if len(list) != len(arguments) {
		return nil, fmt.Errorf("argument count mismatch: got %d, want %d", len(list), len(arguments))
	}
	out := make([]interface{}, len(arguments))
	for i, arg := range arguments {
		c, cerr := coerce(arg.Type, list[i])
		if cerr != nil {
			return nil, fmt.Errorf("arg %d (%s): %w", i, arg.Type.String(), cerr)
		}
		out[i] = c
	}
	return out, nil
}

// toList normalizes args into a []interface{} of JSON-decoded values.
func toList(args any) ([]interface{}, error) {
	switch v := args.(type) {
	case nil:
		return nil, nil
	case []interface{}:
		return v, nil
	case json.RawMessage:
		if len(v) == 0 || string(v) == "null" {
			return nil, nil
		}
		var l []interface{}
		if err := json.Unmarshal(v, &l); err != nil {
			return nil, fmt.Errorf("args must be a JSON array: %w", err)
		}
		return l, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("args must be a JSON array: %w", err)
		}
		var l []interface{}
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("args must be a JSON array: %w", err)
		}
		return l, nil
	}
}

// coerce converts a single JSON value into the Go type required by the ABI type.
func coerce(t abi.Type, v interface{}) (interface{}, error) {
	switch t.T {
	case abi.IntTy, abi.UintTy:
		return coerceInteger(t, v)
	case abi.BoolTy:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", v)
		}
		return b, nil
	case abi.StringTy:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
		return s, nil
	case abi.AddressTy:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected address string, got %T", v)
		}
		if !common.IsHexAddress(s) {
			return nil, fmt.Errorf("invalid address: %q", s)
		}
		return common.HexToAddress(s), nil
	case abi.BytesTy:
		return hexToBytes(v)
	case abi.FixedBytesTy, abi.HashTy:
		return coerceFixedBytes(t, v)
	case abi.SliceTy, abi.ArrayTy:
		return coerceArrayLike(t, v)
	case abi.TupleTy:
		return coerceTuple(t, v)
	default:
		return nil, fmt.Errorf("unsupported abi type: %s", t.String())
	}
}

// coerceInteger returns the exact Go integer type go-ethereum expects for the
// given int/uint size: int8/16/32/64 (or unsigned) for those exact widths,
// *big.Int for everything else (including uint256).
func coerceInteger(t abi.Type, v interface{}) (interface{}, error) {
	bi, err := toBigInt(v)
	if err != nil {
		return nil, err
	}
	signed := t.T == abi.IntTy
	switch t.Size {
	case 8:
		if signed {
			return int8(bi.Int64()), nil
		}
		return uint8(bi.Uint64()), nil
	case 16:
		if signed {
			return int16(bi.Int64()), nil
		}
		return uint16(bi.Uint64()), nil
	case 32:
		if signed {
			return int32(bi.Int64()), nil
		}
		return uint32(bi.Uint64()), nil
	case 64:
		if signed {
			return bi.Int64(), nil
		}
		return bi.Uint64(), nil
	default:
		return bi, nil
	}
}

// toBigInt parses a numeric value from a decimal/hex string, a JSON number, or
// an existing *big.Int. Strings are preferred for values beyond 2^53.
func toBigInt(v interface{}) (*big.Int, error) {
	switch n := v.(type) {
	case *big.Int:
		return n, nil
	case json.Number:
		bi, ok := new(big.Int).SetString(n.String(), 10)
		if !ok {
			return nil, fmt.Errorf("invalid integer: %q", n.String())
		}
		return bi, nil
	case string:
		s := strings.TrimSpace(n)
		base := 10
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			s, base = s[2:], 16
		}
		bi, ok := new(big.Int).SetString(s, base)
		if !ok {
			return nil, fmt.Errorf("invalid integer string: %q", n)
		}
		return bi, nil
	case float64:
		bf := big.NewFloat(n)
		if !bf.IsInt() {
			return nil, fmt.Errorf("non-integral number: %v", n)
		}
		bi, _ := bf.Int(nil)
		return bi, nil
	default:
		return nil, fmt.Errorf("expected number or numeric string, got %T", v)
	}
}

func hexToBytes(v interface{}) ([]byte, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("expected hex string for bytes, got %T", v)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"))
	if err != nil {
		return nil, fmt.Errorf("invalid hex bytes: %w", err)
	}
	return b, nil
}

// coerceFixedBytes builds a fixed-size [N]byte (bytesN / bytes32 / hash).
func coerceFixedBytes(t abi.Type, v interface{}) (interface{}, error) {
	b, err := hexToBytes(v)
	if err != nil {
		return nil, err
	}
	target := t.GetType()
	if len(b) != target.Len() {
		return nil, fmt.Errorf("%s expects %d bytes, got %d", t.String(), target.Len(), len(b))
	}
	arr := reflect.New(target).Elem()
	reflect.Copy(arr, reflect.ValueOf(b))
	return arr.Interface(), nil
}

// coerceArrayLike builds a slice ([]T) or fixed array ([N]T) of coerced elements.
func coerceArrayLike(t abi.Type, v interface{}) (interface{}, error) {
	items, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected JSON array for %s, got %T", t.String(), v)
	}
	target := t.GetType()
	var out reflect.Value
	if t.T == abi.SliceTy {
		out = reflect.MakeSlice(target, len(items), len(items))
	} else {
		if len(items) != target.Len() {
			return nil, fmt.Errorf("array %s expects %d elements, got %d", t.String(), target.Len(), len(items))
		}
		out = reflect.New(target).Elem()
	}
	for i, item := range items {
		c, err := coerce(*t.Elem, item)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out.Index(i).Set(reflect.ValueOf(c))
	}
	return out.Interface(), nil
}

// coerceTuple builds a struct for an ABI tuple. JSON may be an ordered array or
// an object keyed by the solidity field names.
func coerceTuple(t abi.Type, v interface{}) (interface{}, error) {
	target := t.GetType()
	out := reflect.New(target).Elem()
	switch tv := v.(type) {
	case []interface{}:
		if len(tv) != len(t.TupleElems) {
			return nil, fmt.Errorf("tuple expects %d fields, got %d", len(t.TupleElems), len(tv))
		}
		for i, et := range t.TupleElems {
			c, err := coerce(*et, tv[i])
			if err != nil {
				return nil, fmt.Errorf("field %d: %w", i, err)
			}
			out.Field(i).Set(reflect.ValueOf(c))
		}
	case map[string]interface{}:
		for i, et := range t.TupleElems {
			name := t.TupleRawNames[i]
			item, ok := tv[name]
			if !ok {
				return nil, fmt.Errorf("tuple missing field %q", name)
			}
			c, err := coerce(*et, item)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", name, err)
			}
			out.Field(i).Set(reflect.ValueOf(c))
		}
	default:
		return nil, fmt.Errorf("expected JSON object or array for tuple, got %T", v)
	}
	return out.Interface(), nil
}
