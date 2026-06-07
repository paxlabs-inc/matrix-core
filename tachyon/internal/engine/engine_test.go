package engine

import (
	"strings"
	"testing"
)

const erc20ABI = `[{"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]}]`

func TestEncodeContractCall(t *testing.T) {
	data, err := EncodeContractCall([]byte(erc20ABI), "transfer",
		[]interface{}{"0x1111111111111111111111111111111111111111", "1000000000000000000"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// transfer(address,uint256) selector is 0xa9059cbb.
	if !strings.HasPrefix(data, "0xa9059cbb") {
		t.Fatalf("unexpected selector: %s", data)
	}
	// 0x + selector(8) + 2 words(128) = 138 chars.
	if len(data) != 138 {
		t.Fatalf("unexpected calldata length: %d (%s)", len(data), data)
	}
}

func TestEncodeContractCallUnknownMethod(t *testing.T) {
	if _, err := EncodeContractCall([]byte(erc20ABI), "missing", nil); err == nil {
		t.Fatalf("expected error for unknown method")
	}
}
