package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// MaxCanonicalBytes is the hard v1 limit for a canonical contract envelope.
const MaxCanonicalBytes = 2 << 20

// Validatable is implemented by every executable v1 contract.
type Validatable interface {
	Validate() error
}

// EncodeCanonical validates value and returns the deterministic v1 JSON bytes.
// Canonical contracts contain no maps, interfaces, raw JSON, or floating point.
func EncodeCanonical[T Validatable](value T) ([]byte, error) {
	if isNil(value) {
		return nil, fmt.Errorf("canonical encode: nil value")
	}
	if err := validateCanonicalType(reflect.TypeOf(value), make(map[reflect.Type]bool)); err != nil {
		return nil, fmt.Errorf("canonical encode type: %w", err)
	}
	if err := value.Validate(); err != nil {
		return nil, fmt.Errorf("canonical encode validation: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonical encode: %w", err)
	}
	if len(encoded) > MaxCanonicalBytes {
		return nil, fmt.Errorf("canonical encode: payload exceeds %d bytes", MaxCanonicalBytes)
	}
	return encoded, nil
}

// DecodeStrict rejects oversized, malformed, duplicate-key, unknown-field,
// invalid-UTF-8, invalid-schema, trailing, and semantically invalid input.
func DecodeStrict[T any, P interface {
	*T
	Validatable
}](data []byte) (T, error) {
	var zero T
	if len(data) == 0 {
		return zero, fmt.Errorf("canonical decode: empty payload")
	}
	if len(data) > MaxCanonicalBytes {
		return zero, fmt.Errorf("canonical decode: payload exceeds %d bytes", MaxCanonicalBytes)
	}
	if !utf8.Valid(data) {
		return zero, fmt.Errorf("canonical decode: invalid UTF-8")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return zero, fmt.Errorf("canonical decode: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("canonical decode: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return zero, fmt.Errorf("canonical decode: %w", err)
	}
	pointer := P(&value)
	if err := pointer.Validate(); err != nil {
		return zero, fmt.Errorf("canonical decode validation: %w", err)
	}
	return value, nil
}

// DecodeCanonical applies strict decoding and rejects semantically equivalent
// bytes that do not exactly match the canonical v1 representation.
func DecodeCanonical[T any, P interface {
	*T
	Validatable
}](data []byte) (T, error) {
	value, err := DecodeStrict[T, P](data)
	if err != nil {
		return value, err
	}
	canonical, err := EncodeCanonical(P(&value))
	if err != nil {
		var zero T
		return zero, err
	}
	if !bytes.Equal(data, canonical) {
		var zero T
		return zero, fmt.Errorf("canonical decode: non-canonical representation")
	}
	return value, nil
}

// HashCanonical returns SHA-256 over validated canonical plaintext.
func HashCanonical[T Validatable](value T) (ContentHash, error) {
	encoded, err := EncodeCanonical(value)
	if err != nil {
		return ContentHash{}, err
	}
	sum := sha256.Sum256(encoded)
	return ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func isNil[T any](value T) bool {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

func validateCanonicalType(value reflect.Type, seen map[reflect.Type]bool) error {
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
		value = value.Elem()
	}
	if seen[value] {
		return nil
	}
	seen[value] = true
	switch value.Kind() {
	case reflect.Map:
		return fmt.Errorf("map type %s is forbidden", value)
	case reflect.Interface:
		return fmt.Errorf("interface type %s is forbidden", value)
	case reflect.Float32, reflect.Float64:
		return fmt.Errorf("floating-point type %s is forbidden", value)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if field.PkgPath != "" {
				continue
			}
			if strings.Contains(field.Tag.Get("json"), "omitempty") {
				return fmt.Errorf("omitempty is forbidden on %s.%s", value, field.Name)
			}
			if err := validateCanonicalType(field.Type, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object did not terminate")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array did not terminate")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("trailing JSON value")
	}
	return err
}
