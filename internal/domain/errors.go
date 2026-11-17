package domain

import "errors"

var (
	// ErrInvalidAddress means a string is not a 20-byte hex address.
	ErrInvalidAddress = errors.New("invalid address")
	// ErrInvalidHash means a string is not a 32-byte hex hash.
	ErrInvalidHash = errors.New("invalid hash")
	// ErrInvalidInput means a request value failed validation.
	ErrInvalidInput = errors.New("invalid input")
	// ErrNotFound means the requested record does not exist.
	ErrNotFound = errors.New("not found")
	// ErrUnauthenticated means there is no valid session.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden means the caller is signed in but may not act on the resource
	// (not a member of the organization, or not an owner for owner-only actions).
	ErrForbidden = errors.New("forbidden")
	// ErrConflict means the change would break an invariant, e.g. removing an
	// organization's last owner.
	ErrConflict = errors.New("conflict")
	// ErrRangeTooLarge means the RPC node refused an eth_getLogs range because it
	// spans too many blocks or would return too many results. The indexer reacts by
	// halving the range.
	ErrRangeTooLarge = errors.New("log range too large")
)
