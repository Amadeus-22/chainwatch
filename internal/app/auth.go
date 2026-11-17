package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Amadeus-22/chainwatch/internal/domain"
	"github.com/Amadeus-22/chainwatch/internal/domain/ethsig"
	"github.com/Amadeus-22/chainwatch/internal/domain/siwe"
)

// ErrBadNonce means a sign-in message carries a nonce that chainwatch did not
// issue, that was already used, or that expired.
var ErrBadNonce = errors.New("nonce unknown, used or expired")

// AuthConfig tunes Sign-In with Ethereum.
type AuthConfig struct {
	// Domain is the host[:port] users sign in on (the web app's origin); messages
	// for any other domain are rejected, which stops a phishing site from relaying
	// a signature it obtained for itself.
	Domain     string
	ChainIDs   []domain.ChainID // chain ids accepted in the message
	NonceTTL   time.Duration
	SessionTTL time.Duration
	ClockSkew  time.Duration
}

// Auth implements Sign-In with Ethereum (EIP-4361) and sessions.
type Auth struct {
	store  AuthStore
	orgs   OrgStore
	cfg    AuthConfig
	chains map[uint64]bool
	now    func() time.Time
}

// NewAuth wires Auth.
func NewAuth(store AuthStore, orgs OrgStore, cfg AuthConfig) *Auth {
	chains := make(map[uint64]bool, len(cfg.ChainIDs))
	for _, id := range cfg.ChainIDs {
		chains[uint64(id)] = true
	}
	return &Auth{store: store, orgs: orgs, cfg: cfg, chains: chains, now: time.Now}
}

// NewNonce issues a single-use nonce for a sign-in message.
func (a *Auth) NewNonce(ctx context.Context) (nonce string, expiresAt time.Time, err error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, fmt.Errorf("generate nonce: %w", err)
	}
	nonce = hex.EncodeToString(b) // alphanumeric, as EIP-4361 requires
	expiresAt = a.now().Add(a.cfg.NonceTTL)
	if err := a.store.CreateNonce(ctx, nonce, expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("store nonce: %w", err)
	}
	return nonce, expiresAt, nil
}

// SignIn verifies a signed EIP-4361 message and opens a session. Checks, in
// order: message syntax; domain, URI, chain id and time window; the signature
// recovers the message's address; the nonce was issued by us and is consumed
// now (last, so a forged request cannot burn a legitimate user's nonce). An
// account signing in for the first time gets a personal organization.
//
// It returns the bearer token (to be set as an HTTP-only cookie) and the session.
func (a *Auth) SignIn(ctx context.Context, message string, signature []byte) (string, domain.Session, error) {
	m, err := siwe.Parse(message)
	if err != nil {
		return "", domain.Session{}, err
	}
	now := a.now()
	if err := m.Validate(siwe.Expect{Domain: a.cfg.Domain, ChainIDs: a.chains, Now: now, Skew: a.cfg.ClockSkew}); err != nil {
		return "", domain.Session{}, err
	}
	signer, err := ethsig.RecoverAddress(ethsig.PersonalMessageHash([]byte(message)), signature)
	if err != nil {
		return "", domain.Session{}, err
	}
	if signer != m.Address {
		return "", domain.Session{}, fmt.Errorf("%w: signed by %s, message is for %s",
			ethsig.ErrInvalidSignature, ethsig.Checksum(signer), ethsig.Checksum(m.Address))
	}
	if err := a.store.ConsumeNonce(ctx, m.Nonce, now); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return "", domain.Session{}, ErrBadNonce
		}
		return "", domain.Session{}, fmt.Errorf("consume nonce: %w", err)
	}

	if err := a.ensureOrganization(ctx, m.Address); err != nil {
		return "", domain.Session{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", domain.Session{}, fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s := domain.Session{TokenHash: hashToken(token), Address: m.Address, ExpiresAt: now.Add(a.cfg.SessionTTL)}
	if err := a.store.CreateSession(ctx, s); err != nil {
		return "", domain.Session{}, fmt.Errorf("store session: %w", err)
	}
	return token, s, nil
}

func (a *Auth) ensureOrganization(ctx context.Context, account domain.Address) error {
	ms, err := a.orgs.Memberships(ctx, account)
	if err != nil {
		return fmt.Errorf("load memberships of %s: %w", account, err)
	}
	if len(ms) > 0 {
		return nil
	}
	cs := ethsig.Checksum(account)
	if _, err := a.orgs.CreateOrg(ctx, "Workspace "+cs[:6]+"…"+cs[len(cs)-4:], account); err != nil {
		return fmt.Errorf("create personal organization for %s: %w", account, err)
	}
	return nil
}

// Authenticate resolves a session token to the signed-in account.
func (a *Auth) Authenticate(ctx context.Context, token string) (domain.Principal, error) {
	if token == "" {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	s, err := a.store.Session(ctx, hashToken(token), a.now())
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Principal{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.Principal{}, fmt.Errorf("load session: %w", err)
	}
	return domain.Principal{Address: s.Address}, nil
}

// SignOut deletes the session. Signing out of an unknown session is not an error.
func (a *Auth) SignOut(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := a.store.DeleteSession(ctx, hashToken(token)); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func hashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }
