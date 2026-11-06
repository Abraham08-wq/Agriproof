package chain

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

func TestAddressFromAnvilKey(t *testing.T) {
	// Anvil's first public development key and its well-known address.
	b, _ := hex.DecodeString("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	got := AddressFromKey(secp256k1.PrivKeyFromBytes(b))
	if got != "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266" {
		t.Fatalf("unexpected address %s", got)
	}
}

func TestAnchorSelector(t *testing.T) {
	sel := hex.EncodeToString(Keccak256([]byte("anchor(bytes32,bytes32,uint64)"))[:4])
	data := encodeAnchorCall(make([]byte, 32), make([]byte, 32), 7)
	if hex.EncodeToString(data[:4]) != sel || len(data) != 100 || data[99] != 7 {
		t.Fatalf("bad calldata %x", data)
	}
}

func TestRLPKnownVectors(t *testing.T) {
	cases := map[string][]byte{
		"80":       rlpBytes(nil),
		"83646f67": rlpBytes([]byte("dog")),
		"c0":       rlpList(),
		"0f":       rlpBytes([]byte{0x0f}),
	}
	for want, got := range cases {
		if hex.EncodeToString(got) != want {
			t.Errorf("want %s got %x", want, got)
		}
	}
	long := make([]byte, 56)
	if enc := rlpBytes(long); enc[0] != 0xb8 || enc[1] != 56 {
		t.Errorf("long string header wrong: %x", enc[:2])
	}
}
