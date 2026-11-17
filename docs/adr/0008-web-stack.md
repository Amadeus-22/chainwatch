# 0008 — Web app: Vite, React, TypeScript, Tailwind and viem, served by chainwatch

- Status: accepted
- Date: 2026-09-29

## Context

B2B users need a dashboard (wallets per chain, allowances with risk, alerts,
members) and a one-click **Revoke** that sends `approve(spender, 0)` from their own
wallet. The app must never hold keys: MetaMask signs.

## Decision

- `web/`: **Vite + React + TypeScript + Tailwind CSS**, a static single-page app.
  No server-side rendering: nothing is secret in the bundle, and it can be served
  by chainwatch itself (`CHAINWATCH_WEB_DIR`, set in the container image) or any
  CDN behind the same origin as the API.
- **Wallet integration is plain EIP-1193** (`window.ethereum`): `eth_requestAccounts`,
  `eth_chainId`, `wallet_switchEthereumChain`, `personal_sign` and
  `eth_sendTransaction`. No wallet SDK or connector library.
- **viem** only for pure helpers: ABI encoding of `approve`, EIP-55 checksums,
  hex conversion, and as the reference in tests for the SIWE message layout. It
  is tree-shaken; no viem client or transport is used.
- **Same origin**: the Vite dev server proxies `/v1` to the API, and production
  serves both from one origin, so the session cookie is first-party with
  `SameSite=Strict` and no CORS is needed.
- Handled states: no wallet installed, wrong network (sign-in offers to switch to a
  configured chain; revoke switches to the allowance's chain), rejected signature
  or transaction (EIP-1193 code 4001), network unknown to the wallet (4902),
  connected account differs from the allowance owner.
- **Vitest** unit tests cover calldata encoding, the SIWE message builder, wallet
  error mapping and formatting.

## Alternatives considered

- **wagmi / RainbowKit / web3modal**: convenient, but large dependency trees and
  more surface than one injected provider needs; WalletConnect can be added later
  behind the same EIP-1193 interface.
- **ethers.js**: comparable to viem; viem's typed ABI encoding and smaller
  tree-shaken output decided it.
- **Next.js**: SSR and a Node runtime are unnecessary for an authenticated
  dashboard over a JSON API.
- **Go templates / htmx**: would still need JavaScript for EIP-1193 and signing.

## Consequences

- A Node toolchain is needed to build the app (`make web`); the Go service builds
  and runs without it (`CHAINWATCH_WEB_DIR` is optional).
- Token symbols and decimals are not shown yet (amounts are in base units); reading
  them needs an `eth_call` per token and is on the roadmap.
- Revoke works for EOAs; a Safe needs a Safe transaction (roadmap).
