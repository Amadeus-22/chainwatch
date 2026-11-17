package domain_test

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

func TestParseAddress(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"lowercase", "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", false},
		{"checksummed is normalised", "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", false},
		{"upper prefix", "0X70997970c51812dc3a010c7d01b50e0d17dc79c8", "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", false},
		{"no prefix", "70997970c51812dc3a010c7d01b50e0d17dc79c8", "", true},
		{"too short", "0x7099", "", true},
		{"too long", "0x70997970c51812dc3a010c7d01b50e0d17dc79c800", "", true},
		{"not hex", "0xz0997970c51812dc3a010c7d01b50e0d17dc79c8", "", true},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.ParseAddress(tt.in)
			if tt.wantErr {
				if !errors.Is(err, domain.ErrInvalidAddress) {
					t.Fatalf("err = %v, want ErrInvalidAddress", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestParseHash(t *testing.T) {
	const s = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	h, err := domain.ParseHash(s)
	if err != nil || h.String() != s {
		t.Fatalf("ParseHash(%s) = %s, %v", s, h, err)
	}
	if _, err := domain.ParseHash("0x1234"); !errors.Is(err, domain.ErrInvalidHash) {
		t.Fatalf("short hash: err = %v, want ErrInvalidHash", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	type payload struct {
		A domain.Address `json:"a"`
		H domain.Hash    `json:"h"`
	}
	in := payload{A: domain.Address{19: 1}, H: domain.Hash{31: 2}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out payload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip: got %+v, want %+v", out, in)
	}
	if err := json.Unmarshal([]byte(`{"a":"0x12"}`), &out); !errors.Is(err, domain.ErrInvalidAddress) {
		t.Fatalf("bad address: err = %v", err)
	}
}

func TestEventBefore(t *testing.T) {
	a := domain.Event{BlockNumber: 5, LogIndex: 9}
	b := domain.Event{BlockNumber: 6, LogIndex: 0}
	c := domain.Event{BlockNumber: 6, LogIndex: 1}
	if !a.Before(b) || !b.Before(c) || c.Before(a) || b.Before(b) {
		t.Fatal("Before does not order by (block, log index)")
	}
}

func TestNewAlertAndDedupKey(t *testing.T) {
	ev := domain.Event{Token: domain.Address{1}, TxHash: domain.Hash{31: 0xab}, LogIndex: 3, BlockNumber: 7, Amount: big.NewInt(1)}
	a := domain.NewAlert(9, 8453, domain.Address{2}, ev, domain.Finding{Rule: "r", Severity: domain.SeverityHigh, Reason: "why"})
	if a.Status != domain.AlertPending || a.BlockNumber != 7 || a.Token != ev.Token || a.Wallet != (domain.Address{2}) ||
		a.OrgID != 9 || a.ChainID != 8453 {
		t.Fatalf("unexpected alert: %+v", a)
	}
	want := "9:8453:r:0x00000000000000000000000000000000000000000000000000000000000000ab:3"
	if a.DedupKey() != want {
		t.Fatalf("DedupKey = %s, want %s", a.DedupKey(), want)
	}
}
