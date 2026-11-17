package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/Amadeus-22/chainwatch/internal/app"
	"github.com/Amadeus-22/chainwatch/internal/app/apptest"
	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
	"github.com/Amadeus-22/chainwatch/internal/domain/siwe"
)

const appDomain = "app.chainwatch.example"

func testKey(seed byte) *secp256k1.PrivateKey {
	b := make([]byte, 32)
	b[0], b[31] = 0x11, seed
	return secp256k1.PrivKeyFromBytes(b)
}

type authHarness struct {
	auth  *app.Auth
	store *apptest.Accounts
	now   time.Time
}

func newAuthHarness() *authHarness {
	h := &authHarness{store: apptest.NewAccounts(), now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	h.auth = app.NewAuth(h.store, h.store, app.AuthConfig{
		Domain: appDomain, ChainIDs: []domain.ChainID{1, 8453},
		NonceTTL: 10 * time.Minute, SessionTTL: 24 * time.Hour, ClockSkew: time.Minute,
	})
	app.SetAuthClock(h.auth, func() time.Time { return h.now })
	return h
}

// message builds and signs a sign-in message the way the web app and MetaMask do.
func (h *authHarness) message(t *testing.T, key *secp256k1.PrivateKey, nonce string, edit func(*siwe.Message)) (string, []byte) {
	t.Helper()
	exp := h.now.Add(5 * time.Minute)
	m := siwe.Message{
		Domain: appDomain, Address: ethsig.PubkeyToAddress(key.PubKey()), Statement: "Sign in to chainwatch.",
		URI: "https://" + appDomain, Version: "1", ChainID: 8453, Nonce: nonce, IssuedAt: h.now, ExpirationTime: &exp,
	}
	if edit != nil {
		edit(&m)
	}
	text := m.String()
	return text, ethsig.SignPersonal(key, []byte(text))
}

func (h *authHarness) nonce(t *testing.T) string {
	t.Helper()
	n, exp, err := h.auth.NewNonce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(n) < 8 || !exp.Equal(h.now.Add(10*time.Minute)) {
		t.Fatalf("nonce %q expires %s", n, exp)
	}
	return n
}

func TestSignInOpensSessionAndCreatesPersonalOrg(t *testing.T) {
	h := newAuthHarness()
	ctx := context.Background()
	key := testKey(1)
	msg, sig := h.message(t, key, h.nonce(t), nil)

	token, s, err := h.auth.SignIn(ctx, msg, sig)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	want := ethsig.PubkeyToAddress(key.PubKey())
	if s.Address != want || !s.ExpiresAt.Equal(h.now.Add(24*time.Hour)) {
		t.Fatalf("session = %+v", s)
	}
	p, err := h.auth.Authenticate(ctx, token)
	if err != nil || p.Address != want {
		t.Fatalf("Authenticate = %+v, %v", p, err)
	}
	ms, _ := h.store.Memberships(ctx, want)
	if len(ms) != 1 || ms[0].Role != domain.RoleOwner {
		t.Fatalf("memberships after first sign-in = %+v, want one owned organization", ms)
	}

	// A second sign-in does not create another organization.
	msg, sig = h.message(t, key, h.nonce(t), nil)
	if _, _, err := h.auth.SignIn(ctx, msg, sig); err != nil {
		t.Fatal(err)
	}
	if ms, _ := h.store.Memberships(ctx, want); len(ms) != 1 {
		t.Fatalf("memberships after second sign-in = %d", len(ms))
	}

	// The token is stored only as a hash.
	if h.store.HasSessionKey([]byte(token)) {
		t.Fatal("raw token stored")
	}
}

func TestSignInRejects(t *testing.T) {
	ctx := context.Background()
	key, other := testKey(1), testKey(2)
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, h *authHarness) error
		want error
	}{
		{"nonce not issued by us", func(t *testing.T, h *authHarness) error {
			msg, sig := h.message(t, key, "abcdef0123456789", nil)
			_, _, err := h.auth.SignIn(ctx, msg, sig)
			return err
		}, app.ErrBadNonce},
		{"nonce replayed", func(t *testing.T, h *authHarness) error {
			msg, sig := h.message(t, key, h.nonce(t), nil)
			if _, _, err := h.auth.SignIn(ctx, msg, sig); err != nil {
				t.Fatal(err)
			}
			_, _, err := h.auth.SignIn(ctx, msg, sig)
			return err
		}, app.ErrBadNonce},
		{"nonce expired", func(t *testing.T, h *authHarness) error {
			n := h.nonce(t)
			h.now = h.now.Add(11 * time.Minute)
			msg, sig := h.message(t, key, n, nil)
			_, _, err := h.auth.SignIn(ctx, msg, sig)
			return err
		}, app.ErrBadNonce},
		{"message expired", func(t *testing.T, h *authHarness) error {
			msg, sig := h.message(t, key, h.nonce(t), nil)
			h.now = h.now.Add(6 * time.Minute)
			_, _, err := h.auth.SignIn(ctx, msg, sig)
			return err
		}, siwe.ErrInvalidMessage},
		{"wrong domain", func(t *testing.T, h *authHarness) error {
			msg, sig := h.message(t, key, h.nonce(t), func(m *siwe.Message) { m.Domain = "evil.example" })
			_, _, err := h.auth.SignIn(ctx, msg, sig)
			return err
		}, siwe.ErrInvalidMessage},
		{"unconfigured chain", func(t *testing.T, h *authHarness) error {
			msg, sig := h.message(t, key, h.nonce(t), func(m *siwe.Message) { m.ChainID = 137 })
			_, _, err := h.auth.SignIn(ctx, msg, sig)
			return err
		}, siwe.ErrInvalidMessage},
		{"signed by another key", func(t *testing.T, h *authHarness) error {
			msg, _ := h.message(t, key, h.nonce(t), nil)
			_, _, err := h.auth.SignIn(ctx, msg, ethsig.SignPersonal(other, []byte(msg)))
			return err
		}, ethsig.ErrInvalidSignature},
		{"message edited after signing", func(t *testing.T, h *authHarness) error {
			msg, sig := h.message(t, key, h.nonce(t), nil)
			edited := strings.Replace(msg, "Sign in to chainwatch.", "Sign in to chainwatch!", 1)
			_, _, err := h.auth.SignIn(ctx, edited, sig)
			return err
		}, ethsig.ErrInvalidSignature},
		{"garbage signature", func(t *testing.T, h *authHarness) error {
			msg, _ := h.message(t, key, h.nonce(t), nil)
			_, _, err := h.auth.SignIn(ctx, msg, []byte{1, 2, 3})
			return err
		}, ethsig.ErrInvalidSignature},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newAuthHarness()
			if err := tt.run(t, h); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRejectedSignatureDoesNotBurnNonce(t *testing.T) {
	h := newAuthHarness()
	ctx := context.Background()
	n := h.nonce(t)
	msg, _ := h.message(t, testKey(1), n, nil)
	if _, _, err := h.auth.SignIn(ctx, msg, ethsig.SignPersonal(testKey(2), []byte(msg))); err == nil {
		t.Fatal("forged signature accepted")
	}
	msg, sig := h.message(t, testKey(1), n, nil)
	if _, _, err := h.auth.SignIn(ctx, msg, sig); err != nil {
		t.Fatalf("legitimate sign-in after a forged attempt: %v", err)
	}
}

func TestConcurrentReplayWinsOnce(t *testing.T) {
	h := newAuthHarness()
	msg, sig := h.message(t, testKey(1), h.nonce(t), nil)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := h.auth.SignIn(context.Background(), msg, sig); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d concurrent sign-ins with one nonce succeeded, want 1", ok.Load())
	}
}

func TestSessionsExpireAndSignOut(t *testing.T) {
	h := newAuthHarness()
	ctx := context.Background()
	msg, sig := h.message(t, testKey(1), h.nonce(t), nil)
	token, _, err := h.auth.SignIn(ctx, msg, sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.auth.Authenticate(ctx, "not-a-token"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := h.auth.Authenticate(ctx, ""); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("empty token: %v", err)
	}
	if err := h.auth.SignOut(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := h.auth.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("after sign-out: %v", err)
	}

	msg, sig = h.message(t, testKey(1), h.nonce(t), nil)
	token, _, _ = h.auth.SignIn(ctx, msg, sig)
	h.now = h.now.Add(25 * time.Hour)
	if _, err := h.auth.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("after expiry: %v", err)
	}
}
