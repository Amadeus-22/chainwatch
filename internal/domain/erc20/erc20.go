// Package erc20 decodes the two ERC-20 events chainwatch needs straight from log
// topics and data, without an ABI library (docs/adr/0003-no-abi-library.md).
//
// Both events have the shape Event(address indexed a, address indexed b, uint256 v):
// topics[0] is the event signature hash, topics[1] and topics[2] hold the two
// addresses left-padded to 32 bytes, and data is the 32-byte big-endian amount.
package erc20

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// Event signature hashes: keccak256 of the canonical signatures. They are constants
// of the standard, so they are written out rather than computed (computing them
// would need Keccak-256, which is not in the standard library).
var (
	// TopicTransfer is keccak256("Transfer(address,address,uint256)").
	TopicTransfer = mustHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	// TopicApproval is keccak256("Approval(address,address,uint256)").
	TopicApproval = mustHash("0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925")
)

var (
	// ErrNotERC20 means the log is not an ERC-20 Approval or Transfer. ERC-721
	// Transfer and Approval share the same signature hashes but index the token id
	// as a fourth topic and carry no data, so they land here too.
	ErrNotERC20 = errors.New("not an ERC-20 approval or transfer")
	// ErrMalformed means the log claims to be an ERC-20 event but its topics or
	// data are not well formed (e.g. dirty address padding).
	ErrMalformed = errors.New("malformed ERC-20 log")
)

// Decode turns a raw log into an Event. It returns ErrNotERC20 for logs that are
// not ERC-20 Approval/Transfer events, and ErrMalformed for logs that match the
// signature but violate the ABI encoding.
func Decode(l domain.Log) (domain.Event, error) {
	if len(l.Topics) == 0 {
		return domain.Event{}, ErrNotERC20
	}
	var kind domain.EventKind
	switch l.Topics[0] {
	case TopicTransfer:
		kind = domain.KindTransfer
	case TopicApproval:
		kind = domain.KindApproval
	default:
		return domain.Event{}, ErrNotERC20
	}
	if len(l.Topics) != 3 || len(l.Data) != 32 {
		// 4 topics + empty data is the ERC-721 form of the same events.
		return domain.Event{}, ErrNotERC20
	}
	from, err := topicAddress(l.Topics[1])
	if err != nil {
		return domain.Event{}, fmt.Errorf("decode %s topic 1 in tx %s: %w", kind, l.TxHash, err)
	}
	to, err := topicAddress(l.Topics[2])
	if err != nil {
		return domain.Event{}, fmt.Errorf("decode %s topic 2 in tx %s: %w", kind, l.TxHash, err)
	}
	return domain.Event{
		Kind:        kind,
		Token:       l.Address,
		From:        from,
		To:          to,
		Amount:      new(big.Int).SetBytes(l.Data),
		BlockNumber: l.BlockNumber,
		BlockHash:   l.BlockHash,
		TxHash:      l.TxHash,
		LogIndex:    l.LogIndex,
	}, nil
}

// AddressTopic left-pads an address to a 32-byte topic, the form used in
// eth_getLogs topic filters.
func AddressTopic(a domain.Address) domain.Hash {
	var h domain.Hash
	copy(h[12:], a[:])
	return h
}

// topicAddress extracts an address from an indexed topic, rejecting non-zero
// padding: a correct ABI encoder never produces it.
func topicAddress(h domain.Hash) (domain.Address, error) {
	for _, b := range h[:12] {
		if b != 0 {
			return domain.Address{}, fmt.Errorf("%w: address topic %s has non-zero padding", ErrMalformed, h)
		}
	}
	var a domain.Address
	copy(a[:], h[12:])
	return a, nil
}

func mustHash(s string) domain.Hash {
	h, err := domain.ParseHash(s)
	if err != nil {
		panic(err)
	}
	return h
}
