package domain

import "errors"

var (
	// ErrInvalidAddress means a string is not a 20-byte hex address.
	ErrInvalidAddress = errors.New("invalid address")
	// ErrInvalidHash means a string is not a 32-byte hex hash.
	ErrInvalidHash = errors.New("invalid hash")
	// ErrNotFound means the requested record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrRangeTooLarge means the RPC node refused an eth_getLogs range because it
	// spans too many blocks or would return too many results. The indexer reacts by
	// halving the range.
	ErrRangeTooLarge = errors.New("log range too large")
)
