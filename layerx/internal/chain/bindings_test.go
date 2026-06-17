package chain

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestLoadBindingsHasExpectedSurface(t *testing.T) {
	b, err := loadBindings()
	if err != nil {
		t.Fatalf("loadBindings: %v", err)
	}
	if _, ok := b.vault.Methods["settle"]; !ok {
		t.Error("vault abi missing settle")
	}
	if _, ok := b.vault.Methods["reserveBalance"]; !ok {
		t.Error("vault abi missing reserveBalance")
	}
	if _, ok := b.vault.Events["Deposit"]; !ok {
		t.Error("vault abi missing Deposit event")
	}
	if _, ok := b.anchor.Methods["rootOf"]; !ok {
		t.Error("anchor abi missing rootOf")
	}
	if _, ok := b.erc20.Methods["balanceOf"]; !ok {
		t.Error("erc20 abi missing balanceOf")
	}
	if _, ok := b.router.Methods["swapBestRoute"]; !ok {
		t.Error("router abi missing swapBestRoute")
	}
}

func TestPackSettleSelectorAndDeterminism(t *testing.T) {
	b, err := loadBindings()
	if err != nil {
		t.Fatalf("loadBindings: %v", err)
	}
	var batchID, root [32]byte
	batchID[31] = 0x01
	root[31] = 0xab
	batch := Batch{
		BatchId:   batchID,
		Root:      root,
		WindowEnd: 1_700_000_000,
		Payouts: []Payout{
			{Recipient: common.HexToAddress("0x000000000000000000000000000000000000dEaD"), Amount: big.NewInt(1_000_000)},
		},
	}
	data, err := b.PackSettle(batch)
	if err != nil {
		t.Fatalf("PackSettle: %v", err)
	}
	want := b.vault.Methods["settle"].ID
	if !bytes.HasPrefix(data, want) {
		t.Fatalf("selector = %x, want prefix %x", data[:4], want)
	}

	// Deterministic for identical input.
	again, err := b.PackSettle(batch)
	if err != nil {
		t.Fatalf("PackSettle again: %v", err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("identical batch must encode to identical calldata")
	}

	// A different root must change the encoding.
	batch.Root[0] = 0x99
	diff, err := b.PackSettle(batch)
	if err != nil {
		t.Fatalf("PackSettle diff: %v", err)
	}
	if bytes.Equal(data, diff) {
		t.Fatal("different root must change calldata")
	}
}

func TestPackSettleEmptyPayouts(t *testing.T) {
	b, err := loadBindings()
	if err != nil {
		t.Fatalf("loadBindings: %v", err)
	}
	var batchID, root [32]byte
	root[31] = 0x01
	if _, err := b.PackSettle(Batch{BatchId: batchID, Root: root, WindowEnd: 1, Payouts: nil}); err != nil {
		t.Fatalf("PackSettle empty payouts: %v", err)
	}
}

func TestERC20BalanceOfPackUnpack(t *testing.T) {
	b, err := loadBindings()
	if err != nil {
		t.Fatalf("loadBindings: %v", err)
	}
	out := b.erc20.Methods["balanceOf"].Outputs
	packed, err := out.Pack(big.NewInt(123456789))
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	vals, err := out.Unpack(packed)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	got, ok := vals[0].(*big.Int)
	if !ok {
		t.Fatalf("unpacked type %T, want *big.Int", vals[0])
	}
	if got.Int64() != 123456789 {
		t.Fatalf("balance roundtrip = %s, want 123456789", got)
	}
}
