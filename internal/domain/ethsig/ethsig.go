// Package ethsig holds the Ethereum cryptography chainwatch needs to verify
// Sign-In with Ethereum: Keccak-256, EIP-55 checksums, the EIP-191 personal_sign
// digest and secp256k1 public key recovery. It is pure computation; see
// docs/adr/0006-sign-in-with-ethereum.md for the choice of libraries.
package ethsig

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"

	"github.com/Amadeus-22/chainwatch/internal/domain"
)

// ErrInvalidSignature means a signature is malformed or recovers no public key.
var ErrInvalidSignature = errors.New("invalid signature")

// Keccak256 is Ethereum's hash (the original Keccak padding, not NIST SHA3-256).
func Keccak256(data ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Checksum returns the EIP-55 mixed-case form of a.
func Checksum(a domain.Address) string {
	lower := a.String()[2:]
	hash := Keccak256([]byte(lower))
	out := []byte("0x" + lower)
	for i := range lower {
		c := lower[i]
		if c < 'a' || c > 'f' {
			continue
		}
		nibble := hash[i/2]
		if i%2 == 0 {
			nibble >>= 4
		}
		if nibble&0xf >= 8 {
			out[i+2] = c - 'a' + 'A'
		}
	}
	return string(out)
}

// ParseChecksummed parses an address and requires its EIP-55 checksum, as
// EIP-4361 does for the address in a sign-in message.
func ParseChecksummed(s string) (domain.Address, error) {
	a, err := domain.ParseAddress(s)
	if err != nil {
		return domain.Address{}, err
	}
	if Checksum(a) != s {
		return domain.Address{}, fmt.Errorf("%w: %q is not EIP-55 checksummed (want %s)", domain.ErrInvalidAddress, s, Checksum(a))
	}
	return a, nil
}

// PersonalMessageHash is the EIP-191 version 0x45 digest that personal_sign
// (MetaMask, `cast wallet sign`) signs:
// keccak256("\x19Ethereum Signed Message:\n" + len(msg) + msg).
func PersonalMessageHash(msg []byte) [32]byte {
	prefix := "\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg))
	return Keccak256([]byte(prefix), msg)
}

// PubkeyToAddress derives the account address of a public key: the last 20 bytes
// of keccak256 of the uncompressed point without its 0x04 prefix.
func PubkeyToAddress(pub *secp256k1.PublicKey) domain.Address {
	h := Keccak256(pub.SerializeUncompressed()[1:])
	var a domain.Address
	copy(a[:], h[12:])
	return a
}

// RecoverAddress returns the account that produced the 65-byte signature r‖s‖v
// over digest. v may be 27/28 (wallets) or 0/1 (some libraries).
func RecoverAddress(digest [32]byte, sig []byte) (domain.Address, error) {
	if len(sig) != 65 {
		return domain.Address{}, fmt.Errorf("%w: want 65 bytes, got %d", ErrInvalidSignature, len(sig))
	}
	v := sig[64]
	if v >= 27 {
		v -= 27
	}
	if v > 1 {
		return domain.Address{}, fmt.Errorf("%w: recovery id %d", ErrInvalidSignature, sig[64])
	}
	// decred's compact format is header‖r‖s with header 27+recid for an
	// uncompressed key.
	compact := make([]byte, 65)
	compact[0] = 27 + v
	copy(compact[1:], sig[:64])
	pub, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return domain.Address{}, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	return PubkeyToAddress(pub), nil
}

// SignPersonal signs msg like personal_sign and returns r‖s‖v with v in {27, 28}.
// chainwatch never holds user keys; this exists for tests and fixtures.
func SignPersonal(key *secp256k1.PrivateKey, msg []byte) []byte {
	digest := PersonalMessageHash(msg)
	compact := ecdsa.SignCompact(key, digest[:], false)
	sig := make([]byte, 65)
	copy(sig, compact[1:])
	sig[64] = compact[0] // 27 + recid
	return sig
}
