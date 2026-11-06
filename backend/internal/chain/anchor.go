// Package chain anchors ledger head hashes to the ChamaLedger smart contract.
// It signs EIP-155 transactions itself, so the backend needs no heavy Ethereum library.
package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

// Anchorer sends anchor(bytes32 groupId, bytes32 head, uint64 entries) transactions.
type Anchorer struct {
	rpcURL   string
	chainID  *big.Int
	contract []byte // 20 bytes
	key      *secp256k1.PrivateKey
	from     string
	http     *http.Client
	mu       sync.Mutex // one transaction at a time keeps nonces in order
}

func NewAnchorer(rpcURL string, chainID int64, contractHex, privateKeyHex string) (*Anchorer, error) {
	contract, err := decodeHex(contractHex)
	if err != nil || len(contract) != 20 {
		return nil, errors.New("invalid contract address")
	}
	keyBytes, err := decodeHex(privateKeyHex)
	if err != nil || len(keyBytes) != 32 {
		return nil, errors.New("invalid private key")
	}
	key := secp256k1.PrivKeyFromBytes(keyBytes)
	return &Anchorer{
		rpcURL:   rpcURL,
		chainID:  big.NewInt(chainID),
		contract: contract,
		key:      key,
		from:     AddressFromKey(key),
		http:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// From returns the relayer address that must be allowed in the contract.
func (a *Anchorer) From() string { return a.from }

// GroupKey maps a group UUID to the bytes32 used on-chain.
func GroupKey(groupID string) []byte { return Keccak256([]byte(groupID)) }

// Anchor submits the transaction and returns its hash.
func (a *Anchorer) Anchor(ctx context.Context, groupID, headHex string, entries uint64) (string, error) {
	head, err := decodeHex(headHex)
	if err != nil || len(head) != 32 {
		return "", errors.New("head must be a 32-byte hex hash")
	}
	data := encodeAnchorCall(GroupKey(groupID), head, entries)

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.CheckContract(ctx); err != nil {
		return "", err
	}
	nonce, err := a.callQuantity(ctx, "eth_getTransactionCount", a.from, "pending")
	if err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	gasPrice, err := a.callQuantity(ctx, "eth_gasPrice")
	if err != nil {
		return "", fmt.Errorf("gas price: %w", err)
	}
	gas, err := a.callQuantity(ctx, "eth_estimateGas", map[string]string{
		"from": a.from,
		"to":   "0x" + hex.EncodeToString(a.contract),
		"data": "0x" + hex.EncodeToString(data),
	})
	if err != nil {
		return "", fmt.Errorf("estimate gas (is the relayer allowed and the count increasing?): %w", err)
	}
	gas = new(big.Int).Add(gas, new(big.Int).Div(gas, big.NewInt(5))) // +20% headroom

	raw, err := a.signLegacyTx(nonce, gasPrice, gas, data)
	if err != nil {
		return "", err
	}
	var txHash string
	if err := a.rpc(ctx, "eth_sendRawTransaction", &txHash, "0x"+hex.EncodeToString(raw)); err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	return txHash, nil
}

// ErrNoContract means the configured address has no contract on this chain
// (for example, a local blockchain was reset). Anchoring is skipped until it is deployed again.
var ErrNoContract = errors.New("ChamaLedger contract is not deployed at the configured address (run: make deploy-contracts)")

// CheckContract confirms the contract exists, so the backend never "anchors" to an empty address.
func (a *Anchorer) CheckContract(ctx context.Context) error {
	var code string
	if err := a.rpc(ctx, "eth_getCode", &code, "0x"+hex.EncodeToString(a.contract), "latest"); err != nil {
		return err
	}
	if code == "" || code == "0x" {
		return ErrNoContract
	}
	return nil
}

// BlockNumber returns the chain's latest block height.
func (a *Anchorer) BlockNumber(ctx context.Context) (uint64, error) {
	n, err := a.callQuantity(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	return n.Uint64(), nil
}

// Contract returns the configured contract address.
func (a *Anchorer) Contract() string { return "0x" + hex.EncodeToString(a.contract) }

// ChainID returns the configured chain id.
func (a *Anchorer) ChainID() int64 { return a.chainID.Int64() }

// Latest reads the group's most recent anchor straight from the smart contract,
// so verification does not depend on the backend's own database.
func (a *Anchorer) Latest(ctx context.Context, groupID string) (head string, entries uint64, err error) {
	if err := a.CheckContract(ctx); err != nil {
		return "", 0, err
	}
	data := append(Keccak256([]byte("latest(bytes32)"))[:4], GroupKey(groupID)...)
	var out string
	err = a.rpc(ctx, "eth_call", &out, map[string]string{
		"to":   "0x" + hex.EncodeToString(a.contract),
		"data": "0x" + hex.EncodeToString(data),
	}, "latest")
	if err != nil {
		return "", 0, err
	}
	raw, err := decodeHex(out)
	if err != nil || len(raw) < 96 {
		return "", 0, fmt.Errorf("unexpected eth_call result %q", out)
	}
	head = hex.EncodeToString(raw[0:32])
	entries = new(big.Int).SetBytes(raw[32:64]).Uint64()
	return head, entries, nil
}

func encodeAnchorCall(groupKey, head []byte, entries uint64) []byte {
	selector := Keccak256([]byte("anchor(bytes32,bytes32,uint64)"))[:4]
	out := make([]byte, 0, 4+96)
	out = append(out, selector...)
	out = append(out, groupKey...)
	out = append(out, head...)
	word := make([]byte, 32)
	new(big.Int).SetUint64(entries).FillBytes(word)
	out = append(out, word...)
	return out
}

// signLegacyTx builds and signs an EIP-155 transaction.
func (a *Anchorer) signLegacyTx(nonce, gasPrice, gas *big.Int, data []byte) ([]byte, error) {
	unsigned := rlpList(
		rlpBigInt(nonce), rlpBigInt(gasPrice), rlpBigInt(gas),
		rlpBytes(a.contract), rlpBigInt(big.NewInt(0)), rlpBytes(data),
		rlpBigInt(a.chainID), rlpBigInt(big.NewInt(0)), rlpBigInt(big.NewInt(0)),
	)
	sigHash := Keccak256(unsigned)
	compact := ecdsa.SignCompact(a.key, sigHash, false) // [27+recID][R 32][S 32]
	recID := int64(compact[0]) - 27
	if recID < 0 || recID > 3 {
		return nil, errors.New("unexpected signature recovery id")
	}
	r := new(big.Int).SetBytes(compact[1:33])
	s := new(big.Int).SetBytes(compact[33:65])
	v := new(big.Int).Add(new(big.Int).Mul(a.chainID, big.NewInt(2)), big.NewInt(35+recID))

	return rlpList(
		rlpBigInt(nonce), rlpBigInt(gasPrice), rlpBigInt(gas),
		rlpBytes(a.contract), rlpBigInt(big.NewInt(0)), rlpBytes(data),
		rlpBigInt(v), rlpBigInt(r), rlpBigInt(s),
	), nil
}

// --- JSON-RPC ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Anchorer) rpc(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.rpcURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("rpc %s: %s", method, r.Error.Message)
	}
	return json.Unmarshal(r.Result, out)
}

func (a *Anchorer) callQuantity(ctx context.Context, method string, params ...any) (*big.Int, error) {
	var s string
	if err := a.rpc(ctx, method, &s, params...); err != nil {
		return nil, err
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("bad quantity %q", s)
	}
	return n, nil
}

// --- helpers ---

func Keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

func AddressFromKey(key *secp256k1.PrivateKey) string {
	pub := key.PubKey().SerializeUncompressed() // 0x04 || X || Y
	return "0x" + hex.EncodeToString(Keccak256(pub[1:])[12:])
}

func decodeHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
}

// --- minimal RLP encoding ---

func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	return append(rlpHeader(0x80, len(b)), b...)
}

func rlpBigInt(n *big.Int) []byte {
	if n.Sign() == 0 {
		return []byte{0x80}
	}
	return rlpBytes(n.Bytes())
}

func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	return append(rlpHeader(0xc0, len(payload)), payload...)
}

func rlpHeader(offset byte, length int) []byte {
	if length < 56 {
		return []byte{offset + byte(length)}
	}
	lenBytes := new(big.Int).SetInt64(int64(length)).Bytes()
	return append([]byte{offset + 55 + byte(len(lenBytes))}, lenBytes...)
}
