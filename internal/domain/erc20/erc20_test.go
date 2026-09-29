package erc20_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/erc20"
)

// rpcLog is the eth_getLogs JSON shape of the fixtures, which were captured from
// real nodes: anvil running contracts/script/Drain.s.sol, and Ethereum mainnet.
type rpcLog struct {
	Address     string   `json:"address"`
	Topics      []string `json:"topics"`
	Data        string   `json:"data"`
	BlockNumber string   `json:"blockNumber"`
	BlockHash   string   `json:"blockHash"`
	TxHash      string   `json:"transactionHash"`
	LogIndex    string   `json:"logIndex"`
}

func (r rpcLog) toDomain(t *testing.T) domain.Log {
	t.Helper()
	var l domain.Log
	var err error
	if l.Address, err = domain.ParseAddress(r.Address); err != nil {
		t.Fatal(err)
	}
	for _, s := range r.Topics {
		h, err := domain.ParseHash(s)
		if err != nil {
			t.Fatal(err)
		}
		l.Topics = append(l.Topics, h)
	}
	if l.Data, err = hex.DecodeString(strings.TrimPrefix(r.Data, "0x")); err != nil {
		t.Fatal(err)
	}
	if l.BlockHash, err = domain.ParseHash(r.BlockHash); err != nil {
		t.Fatal(err)
	}
	if l.TxHash, err = domain.ParseHash(r.TxHash); err != nil {
		t.Fatal(err)
	}
	l.BlockNumber = mustQuantity(t, r.BlockNumber)
	l.LogIndex = uint(mustQuantity(t, r.LogIndex))
	return l
}

func mustQuantity(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func addr(t *testing.T, s string) domain.Address {
	t.Helper()
	a, err := domain.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDecodeAnvilDrainScenario(t *testing.T) {
	var logs []rpcLog
	loadJSON(t, "testdata/anvil_drain_logs.json", &logs)

	token := addr(t, "0x5fbdb2315678afecb367f032d93f642f64180aa3")
	victim := addr(t, "0x70997970c51812dc3a010c7d01b50e0d17dc79c8")
	scammer := addr(t, "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc")
	thousand, _ := new(big.Int).SetString("1000000000000000000000", 10)
	maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

	want := []domain.Event{
		{Kind: domain.KindTransfer, Token: token, From: domain.Address{}, To: victim, Amount: thousand, BlockNumber: 1},
		{Kind: domain.KindApproval, Token: token, From: victim, To: scammer, Amount: maxUint256, BlockNumber: 2},
		{Kind: domain.KindTransfer, Token: token, From: victim, To: scammer, Amount: thousand, BlockNumber: 3},
	}
	if len(logs) != len(want) {
		t.Fatalf("fixture has %d logs, want %d", len(logs), len(want))
	}
	for i, rl := range logs {
		l := rl.toDomain(t)
		got, err := erc20.Decode(l)
		if err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
		w := want[i]
		if got.Kind != w.Kind || got.Token != w.Token || got.From != w.From || got.To != w.To ||
			got.Amount.Cmp(w.Amount) != 0 || got.BlockNumber != w.BlockNumber {
			t.Errorf("log %d: got %+v, want %+v", i, got, w)
		}
		if got.TxHash != l.TxHash || got.BlockHash != l.BlockHash || got.LogIndex != l.LogIndex {
			t.Errorf("log %d: position fields not carried over", i)
		}
	}
}

func TestDecodeMainnetFixtures(t *testing.T) {
	var fx map[string]rpcLog
	loadJSON(t, "testdata/mainnet_logs.json", &fx)
	usdc := addr(t, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48")

	t.Run("USDC approval", func(t *testing.T) {
		got, err := erc20.Decode(fx["usdc_approval"].toDomain(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != domain.KindApproval || got.Token != usdc ||
			got.From != addr(t, "0xb92fe925dc43a0ecde6c8b1a2709c170ec4fff4f") ||
			got.To != addr(t, "0x6131b5fae19ea4f9d964eac0408e4408b66337b5") ||
			got.Amount.Cmp(big.NewInt(0x2729c259a)) != 0 || got.LogIndex != 0x1ee {
			t.Errorf("unexpected event %+v", got)
		}
	})
	t.Run("USDC transfer", func(t *testing.T) {
		got, err := erc20.Decode(fx["usdc_transfer"].toDomain(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != domain.KindTransfer || got.Amount.Cmp(big.NewInt(0x411ca)) != 0 ||
			got.To != addr(t, "0x0000000aa232009084bd71a5797d089aa4edfad4") {
			t.Errorf("unexpected event %+v", got)
		}
	})
	t.Run("ERC-721 transfer is rejected", func(t *testing.T) {
		_, err := erc20.Decode(fx["erc721_transfer"].toDomain(t))
		if !errors.Is(err, erc20.ErrNotERC20) {
			t.Fatalf("err = %v, want ErrNotERC20", err)
		}
	})
}

func TestDecodeRejects(t *testing.T) {
	owner := erc20.AddressTopic(domain.Address{19: 1})
	spender := erc20.AddressTopic(domain.Address{19: 2})
	amount := make([]byte, 32)
	dirty := spender
	dirty[0] = 0xff
	other := domain.Hash{1}

	tests := []struct {
		name string
		log  domain.Log
		want error
	}{
		{"no topics", domain.Log{Data: amount}, erc20.ErrNotERC20},
		{"unknown signature", domain.Log{Topics: []domain.Hash{other, owner, spender}, Data: amount}, erc20.ErrNotERC20},
		{"two topics", domain.Log{Topics: []domain.Hash{erc20.TopicApproval, owner}, Data: amount}, erc20.ErrNotERC20},
		{"short data", domain.Log{Topics: []domain.Hash{erc20.TopicTransfer, owner, spender}, Data: amount[:31]}, erc20.ErrNotERC20},
		{"dirty padding", domain.Log{Topics: []domain.Hash{erc20.TopicTransfer, owner, dirty}, Data: amount}, erc20.ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := erc20.Decode(tt.log); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestAddressTopic(t *testing.T) {
	a := addr(t, "0x70997970c51812dc3a010c7d01b50e0d17dc79c8")
	want := "0x00000000000000000000000070997970c51812dc3a010c7d01b50e0d17dc79c8"
	if got := erc20.AddressTopic(a).String(); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
