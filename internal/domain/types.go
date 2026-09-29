// Package domain holds chainwatch's core types and sentinel errors. It performs no
// I/O and imports nothing from other layers.
package domain

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Address is a 20-byte Ethereum account or contract address.
type Address [20]byte

// ParseAddress accepts a 0x-prefixed, 40-hex-digit address in any letter case.
// The EIP-55 checksum is not verified: computing it needs Keccak-256, which is not
// in the standard library (see docs/adr/0003-no-abi-library.md).
func ParseAddress(s string) (Address, error) {
	var a Address
	if err := decodeFixedHex(s, a[:]); err != nil {
		return Address{}, fmt.Errorf("%w: %q: %v", ErrInvalidAddress, s, err)
	}
	return a, nil
}

// String returns the lowercase 0x-prefixed hex form.
func (a Address) String() string { return "0x" + hex.EncodeToString(a[:]) }

// IsZero reports whether a is the zero address.
func (a Address) IsZero() bool { return a == Address{} }

// MarshalText implements encoding.TextMarshaler (used by encoding/json).
func (a Address) MarshalText() ([]byte, error) { return []byte(a.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Address) UnmarshalText(b []byte) error {
	parsed, err := ParseAddress(string(b))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// Hash is a 32-byte Keccak-256 hash: a block hash, transaction hash or log topic.
type Hash [32]byte

// ParseHash accepts a 0x-prefixed, 64-hex-digit hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	if err := decodeFixedHex(s, h[:]); err != nil {
		return Hash{}, fmt.Errorf("%w: %q: %v", ErrInvalidHash, s, err)
	}
	return h, nil
}

// String returns the lowercase 0x-prefixed hex form.
func (h Hash) String() string { return "0x" + hex.EncodeToString(h[:]) }

// MarshalText implements encoding.TextMarshaler.
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (h *Hash) UnmarshalText(b []byte) error {
	parsed, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = parsed
	return nil
}

func decodeFixedHex(s string, dst []byte) error {
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return fmt.Errorf("missing 0x prefix")
	}
	body := s[2:]
	if len(body) != 2*len(dst) {
		return fmt.Errorf("want %d hex digits, got %d", 2*len(dst), len(body))
	}
	if _, err := hex.Decode(dst, []byte(body)); err != nil {
		return err
	}
	return nil
}

// Log is a raw EVM log as returned by eth_getLogs.
type Log struct {
	Address     Address // emitting contract
	Topics      []Hash
	Data        []byte
	BlockNumber uint64
	BlockHash   Hash
	TxHash      Hash
	LogIndex    uint
	Removed     bool // true when the node reports the log was reorged out
}

// EventKind distinguishes the two ERC-20 events chainwatch understands.
type EventKind string

const (
	KindApproval EventKind = "approval"
	KindTransfer EventKind = "transfer"
)

// Event is a decoded ERC-20 Approval or Transfer.
//
// For an Approval, From is the owner and To the spender. For a Transfer, From is the
// sender and To the recipient. Amount is never nil.
type Event struct {
	Kind        EventKind
	Token       Address
	From        Address
	To          Address
	Amount      *big.Int
	BlockNumber uint64
	BlockHash   Hash
	TxHash      Hash
	LogIndex    uint
}

// Before orders events by chain position (block, then log index).
func (e Event) Before(o Event) bool {
	if e.BlockNumber != o.BlockNumber {
		return e.BlockNumber < o.BlockNumber
	}
	return e.LogIndex < o.LogIndex
}

// Wallet is an address on the watch list.
type Wallet struct {
	Address   Address   `json:"address"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// Allowance is the last approved amount an owner granted a spender on a token: the
// owner's current exposure to that spender, as far as Approval events tell.
type Allowance struct {
	Owner        Address
	Token        Address
	Spender      Address
	Amount       *big.Int
	UpdatedBlock uint64
}

// Checkpoint is the last fully processed block and the hash it had at the time.
type Checkpoint struct {
	Block uint64
	Hash  Hash
}

// Severity ranks findings.
type Severity string

const (
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Finding is the output of one risk rule for one event.
type Finding struct {
	Rule     string
	Severity Severity
	Reason   string
}

// AlertStatus tracks delivery of an alert.
type AlertStatus string

const (
	AlertPending   AlertStatus = "pending"
	AlertDelivered AlertStatus = "delivered"
)

// Alert is a persisted finding about a watched wallet. It is unique per
// (Rule, TxHash, LogIndex), which makes re-indexing a range harmless.
type Alert struct {
	ID          int64
	Wallet      Address
	Rule        string
	Severity    Severity
	Reason      string
	Token       Address
	TxHash      Hash
	LogIndex    uint
	BlockNumber uint64
	Status      AlertStatus
	Attempts    int
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

// DedupKey identifies the alert across processes and retries. Notifiers send it so
// receivers can drop a redelivery after a crash between send and acknowledgement.
func (a Alert) DedupKey() string {
	return fmt.Sprintf("%s:%s:%d", a.Rule, a.TxHash, a.LogIndex)
}

// NewAlert builds a pending alert for wallet from a finding on event ev.
func NewAlert(wallet Address, ev Event, f Finding) Alert {
	return Alert{
		Wallet:      wallet,
		Rule:        f.Rule,
		Severity:    f.Severity,
		Reason:      f.Reason,
		Token:       ev.Token,
		TxHash:      ev.TxHash,
		LogIndex:    ev.LogIndex,
		BlockNumber: ev.BlockNumber,
		Status:      AlertPending,
	}
}
