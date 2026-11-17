# 0006 — Sign-In with Ethereum, and the two crypto dependencies it needs

- Status: accepted
- Date: 2026-09-29

## Context

The users are wallet operators. Their identity is the address they sign with, and
the web app already talks to MetaMask to revoke allowances. Passwords or e-mail
would add a second identity to secure and link.

## Decision

Authenticate with **EIP-4361 (Sign-In with Ethereum)**:

1. `GET /v1/auth/nonce` issues a random 128-bit nonce, stored with a 10-minute
   expiry.
2. The browser builds the EIP-4361 message and the wallet signs it with
   `personal_sign` (EIP-191).
3. `POST /v1/auth/verify` checks, in order: strict parsing (field order, EIP-55
   address, version 1, alphanumeric nonce); **domain** equals the configured
   `CHAINWATCH_SIWE_DOMAIN` and the URI is on it (a phishing site cannot relay a
   signature made for itself); **chain id** is a configured chain; **time**
   (issued-at not in the future beyond a 1-minute skew, expiration and
   not-before); the signature **recovers** the message's address; then the nonce
   is **consumed** with one conditional `UPDATE`, so it is single-use even under
   concurrent requests. The nonce is consumed last, so a forged request cannot
   burn a legitimate user's nonce.
4. A 256-bit random session token goes into an `HttpOnly`, `SameSite=Strict`
   cookie (`Secure` unless `CHAINWATCH_COOKIE_SECURE=false` for local http). Only
   its SHA-256 is stored. Unsafe requests with an `Origin` from another host are
   refused (`403 cross_origin`).
5. A first sign-in creates a personal organization owned by the account.

Message parsing is hand-written (`internal/domain/siwe`, about 300 lines); the format
is small and a strict parser is easier to audit than a generic one.

### Dependencies (exception to the allowed list in ENGINEERING.md)

Verifying a signature needs Keccak-256 and secp256k1 public key recovery. Neither
is in the standard library (`crypto/sha3` is NIST SHA-3, which pads differently;
`crypto/ecdsa` has no secp256k1 curve and no recovery).

- **`golang.org/x/crypto`** — only `sha3.NewLegacyKeccak256`. Maintained by the Go
  team, already in the module graph through pgx, no cgo.
- **`github.com/decred/dcrd/dcrec/secp256k1/v4`** — `ecdsa.RecoverCompact`. Pure
  Go, constant-time field arithmetic, no dependencies of its own, used by btcd and
  by go-ethereum itself when built without cgo. Audited as part of dcrd.

Both are wrapped in `internal/domain/ethsig` (Keccak, EIP-55, EIP-191 digest,
recovery), the only package importing them.

## Alternatives considered

- **go-ethereum** (`crypto`, `accounts`): the same functions plus hundreds of
  transitive packages and cgo by default; ADR 0003 already rejected it.
- **A SIWE library** (e.g. `spruceid/siwe-go`): pulls in go-ethereum.
- **Implementing Keccak/secp256k1 by hand**: never for signature verification.
- **Wallet-agnostic auth (OAuth, magic links)**: a second identity to secure,
  with no link to the wallets being protected.

## Consequences

- Compatibility is tested against keys generated in tests and against a fixture
  produced by Foundry's `cast wallet sign` (`internal/domain/siwe/testdata`), and
  the web app's message builder is tested against viem's `createSiweMessage`.
- Smart-contract wallets (EIP-1271, e.g. Safe) cannot sign in yet; members sign in
  with their own EOA and watch the Safe's address. EIP-1271 verification needs an
  `eth_call` and is on the roadmap.
- `ENGINEERING.md` lists the two modules as allowed for chainwatch, pointing here.
