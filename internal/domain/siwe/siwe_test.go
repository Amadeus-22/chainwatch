package siwe_test

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
	"github.com/Amadeus-22/chainwatch/internal/domain/siwe"
)

// anvil dev account #0 (public test key); it signed testdata/cast_signature.txt.
var anvilAddr = mustAddr("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")

func mustAddr(s string) domain.Address {
	a, err := domain.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return a
}

var issued = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func sample() siwe.Message {
	exp := issued.Add(10 * time.Minute)
	return siwe.Message{
		Domain: "app.chainwatch.example", Address: anvilAddr, Statement: "Sign in to chainwatch.",
		URI: "https://app.chainwatch.example", Version: "1", ChainID: 8453, Nonce: "3f9a1c0b7e2d4a6f",
		IssuedAt: issued, ExpirationTime: &exp,
	}
}

func expect() siwe.Expect {
	return siwe.Expect{Domain: "app.chainwatch.example", ChainIDs: map[uint64]bool{1: true, 8453: true},
		Now: issued.Add(time.Minute), Skew: time.Minute}
}

// The fixture was produced by Foundry, i.e. by an implementation independent of
// this package:
//
//	cast wallet sign --private-key 0xac09…ff80 "$(cat testdata/cast_message.txt)"
func TestCastSignedFixture(t *testing.T) {
	msg, err := os.ReadFile("testdata/cast_message.txt")
	if err != nil {
		t.Fatal(err)
	}
	sigHex, err := os.ReadFile("testdata/cast_signature.txt")
	if err != nil {
		t.Fatal(err)
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(sigHex)), "0x"))
	if err != nil {
		t.Fatal(err)
	}

	m, err := siwe.Parse(string(msg))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.String() != string(msg) {
		t.Fatalf("String() does not round-trip:\n%s\n---\n%s", m.String(), msg)
	}
	if err := m.Validate(expect()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	signer, err := ethsig.RecoverAddress(ethsig.PersonalMessageHash(msg), sig)
	if err != nil {
		t.Fatal(err)
	}
	if signer != m.Address {
		t.Fatalf("cast signature recovers %s, message says %s", signer, m.Address)
	}
}

func TestParseRoundTripsOptionalFields(t *testing.T) {
	nb := issued.Add(-time.Minute)
	for name, m := range map[string]siwe.Message{
		"full": sample(),
		"no statement, no expiry": func() siwe.Message {
			m := sample()
			m.Statement, m.ExpirationTime = "", nil
			return m
		}(),
		"everything": func() siwe.Message {
			m := sample()
			m.Scheme, m.NotBefore, m.RequestID = "https", &nb, "req-1"
			m.Resources = []string{"ipfs://bafy", "https://example.com/a"}
			return m
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			text := m.String()
			got, err := siwe.Parse(text)
			if err != nil {
				t.Fatalf("Parse(%q): %v", text, err)
			}
			if got.String() != text {
				t.Fatalf("round trip differs:\n%s\n---\n%s", got.String(), text)
			}
		})
	}
}

func TestNoStatementLayout(t *testing.T) {
	m := sample()
	m.Statement = ""
	// EIP-4361: address LF, LF, (no statement), LF, "URI: ".
	if !strings.Contains(m.String(), "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266\n\n\nURI: ") {
		t.Fatalf("unexpected layout:\n%s", m.String())
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	good := sample().String()
	for name, text := range map[string]string{
		"empty":             "",
		"bad header":        strings.Replace(good, "wants you to sign in", "wants you to log in", 1),
		"lowercase address": strings.Replace(good, "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266", 1),
		"version 2":         strings.Replace(good, "Version: 1", "Version: 2", 1),
		"short nonce":       strings.Replace(good, "Nonce: 3f9a1c0b7e2d4a6f", "Nonce: abc", 1),
		"symbol nonce":      strings.Replace(good, "Nonce: 3f9a1c0b7e2d4a6f", "Nonce: 3f9a1c0b-e2d4a6f", 1),
		"bad chain":         strings.Replace(good, "Chain ID: 8453", "Chain ID: base", 1),
		"bad time":          strings.Replace(good, "Issued At: 2026-09-29T12:00:00Z", "Issued At: yesterday", 1),
		"fields reordered":  strings.Replace(good, "Version: 1\nChain ID: 8453", "Chain ID: 8453\nVersion: 1", 1),
		"trailing junk":     good + "\nFoo: bar",
		"CRLF":              strings.ReplaceAll(good, "\n", "\r\n"),
		"relative URI":      strings.Replace(good, "URI: https://app.chainwatch.example", "URI: /login", 1),
		"too long":          good + strings.Repeat("x", siwe.MaxMessageBytes),
	} {
		if _, err := siwe.Parse(text); !errors.Is(err, siwe.ErrInvalidMessage) {
			t.Errorf("%s: err = %v, want ErrInvalidMessage", name, err)
		}
	}
}

func TestValidate(t *testing.T) {
	future := issued.Add(time.Hour)
	for _, tt := range []struct {
		name string
		edit func(*siwe.Message, *siwe.Expect)
		ok   bool
	}{
		{"valid", func(*siwe.Message, *siwe.Expect) {}, true},
		{"other domain (phishing site relaying the message)", func(m *siwe.Message, _ *siwe.Expect) { m.Domain = "evil.example" }, false},
		{"URI on another host", func(m *siwe.Message, _ *siwe.Expect) { m.URI = "https://evil.example" }, false},
		{"unconfigured chain", func(m *siwe.Message, _ *siwe.Expect) { m.ChainID = 10 }, false},
		{"expired", func(_ *siwe.Message, e *siwe.Expect) { e.Now = issued.Add(10 * time.Minute) }, false},
		{"issued in the future", func(m *siwe.Message, _ *siwe.Expect) { m.IssuedAt = future }, false},
		{"issued slightly ahead, within skew", func(m *siwe.Message, _ *siwe.Expect) { m.IssuedAt = issued.Add(90 * time.Second) }, true},
		{"not yet valid", func(m *siwe.Message, _ *siwe.Expect) { m.NotBefore = &future }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, e := sample(), expect()
			tt.edit(&m, &e)
			err := m.Validate(e)
			if tt.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.ok && !errors.Is(err, siwe.ErrInvalidMessage) {
				t.Fatalf("err = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestSignatureFromGeneratedKeys(t *testing.T) {
	for _, seed := range []byte{1, 2, 3, 0x42} {
		kb := make([]byte, 32)
		kb[31] = seed
		k := secp256k1.PrivKeyFromBytes(kb)
		m := sample()
		m.Address = ethsig.PubkeyToAddress(k.PubKey())
		text := m.String()
		parsed, err := siwe.Parse(text)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		signer, err := ethsig.RecoverAddress(ethsig.PersonalMessageHash([]byte(text)), ethsig.SignPersonal(k, []byte(text)))
		if err != nil || signer != parsed.Address {
			t.Fatalf("seed %d: recovered %s (%v), want %s", seed, signer, err, parsed.Address)
		}
	}
}

// The web app (web/src/lib/siwe.ts, checked against viem's createSiweMessage)
// writes timestamps with milliseconds, as Date.toISOString does.
func TestParsesWebAppMessage(t *testing.T) {
	text := strings.Join([]string{
		"app.chainwatch.example wants you to sign in with your Ethereum account:",
		"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		"",
		"Sign in to chainwatch.",
		"",
		"URI: https://app.chainwatch.example",
		"Version: 1",
		"Chain ID: 8453",
		"Nonce: 3f9a1c0b7e2d4a6f",
		"Issued At: 2026-09-29T12:00:00.000Z",
		"Expiration Time: 2026-09-29T12:10:00.000Z",
	}, "\n")
	m, err := siwe.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IssuedAt.Equal(issued) || m.ExpirationTime == nil || !m.ExpirationTime.Equal(issued.Add(10*time.Minute)) {
		t.Fatalf("times = %s, %v", m.IssuedAt, m.ExpirationTime)
	}
	if err := m.Validate(expect()); err != nil {
		t.Fatal(err)
	}
}
