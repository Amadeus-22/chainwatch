package ethsig_test

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
)

// anvil dev account #0: public test key, never use it elsewhere.
const (
	anvilKey  = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	anvilAddr = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
)

func key(t *testing.T, h string) *secp256k1.PrivateKey {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return secp256k1.PrivKeyFromBytes(b)
}

func TestKeccak256KnownVectors(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
		"Transfer(address,address,uint256)": "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
	} {
		got := ethsig.Keccak256([]byte(in))
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("Keccak256(%q) = %x, want %s", in, got, want)
		}
	}
}

func TestChecksumEIP55Vectors(t *testing.T) {
	// From the EIP-55 specification.
	for _, want := range []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		a, err := domain.ParseAddress(want)
		if err != nil {
			t.Fatal(err)
		}
		if got := ethsig.Checksum(a); got != want {
			t.Errorf("Checksum = %s, want %s", got, want)
		}
		if _, err := ethsig.ParseChecksummed(want); err != nil {
			t.Errorf("ParseChecksummed(%s): %v", want, err)
		}
	}
	if _, err := ethsig.ParseChecksummed("0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"); !errors.Is(err, domain.ErrInvalidAddress) {
		t.Errorf("lowercase address accepted as checksummed: %v", err)
	}
}

func TestPubkeyToAddressOfKnownKey(t *testing.T) {
	got := ethsig.PubkeyToAddress(key(t, anvilKey).PubKey())
	if ethsig.Checksum(got) != anvilAddr {
		t.Fatalf("address = %s, want %s", ethsig.Checksum(got), anvilAddr)
	}
}

func TestSignAndRecoverRoundTrip(t *testing.T) {
	k := key(t, anvilKey)
	msg := []byte("hello chainwatch")
	sig := ethsig.SignPersonal(k, msg)
	if sig[64] != 27 && sig[64] != 28 {
		t.Fatalf("v = %d, want 27 or 28", sig[64])
	}
	got, err := ethsig.RecoverAddress(ethsig.PersonalMessageHash(msg), sig)
	if err != nil {
		t.Fatal(err)
	}
	if ethsig.Checksum(got) != anvilAddr {
		t.Fatalf("recovered %s, want %s", ethsig.Checksum(got), anvilAddr)
	}

	// v as 0/1 is accepted too.
	low := append([]byte(nil), sig...)
	low[64] -= 27
	if got2, err := ethsig.RecoverAddress(ethsig.PersonalMessageHash(msg), low); err != nil || got2 != got {
		t.Fatalf("v in {0,1}: got %s, %v", got2, err)
	}

	// A different message recovers a different (wrong) account.
	other, err := ethsig.RecoverAddress(ethsig.PersonalMessageHash([]byte("tampered")), sig)
	if err == nil && other == got {
		t.Fatal("tampered message recovered the signer")
	}
}

func TestRecoverRejectsMalformed(t *testing.T) {
	digest := ethsig.PersonalMessageHash([]byte("x"))
	for name, sig := range map[string][]byte{
		"short":    make([]byte, 64),
		"bad v":    append(make([]byte, 64), 5),
		"zero r,s": append(make([]byte, 64), 27),
	} {
		if _, err := ethsig.RecoverAddress(digest, sig); !errors.Is(err, ethsig.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}
