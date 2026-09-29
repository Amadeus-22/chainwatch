# 0003 — No ABI library: decode the two ERC-20 events by hand

- Status: accepted
- Date: 2026-09-29

## Context

chainwatch decodes exactly two events, both with the same layout:

```
Transfer(address indexed from,  address indexed to,      uint256 value)
Approval(address indexed owner, address indexed spender, uint256 value)
```

The usual Go option is `go-ethereum` (`accounts/abi`, `common`, `crypto`), which
pulls in a large dependency tree (and cgo for secp256k1 unless tags are set) to
decode 3 topics and 32 bytes.

## Decision

Decode by hand in `internal/domain/erc20`:

- `topics[0]` is compared with the two signature hashes, written as constants
  (they are fixed by the standard; computing them would need Keccak-256, which is
  not in the standard library);
- `topics[1]` and `topics[2]` are addresses left-padded to 32 bytes; non-zero
  padding is rejected as malformed;
- `data` must be exactly 32 bytes, read as a big-endian `uint256` into `*big.Int`.

A minimal JSON-RPC client (`internal/adapter/ethrpc`) replaces `ethclient`.

## Consequences

- No dependency outside the allowed list; the domain package is ~100 lines and
  fully tested with **real** logs captured from anvil (the drain script) and from
  Ethereum mainnet (USDC `Approval`/`Transfer`).
- ERC-721 `Transfer`/`Approval` share the same signature hashes but have four
  topics and empty data; they are recognised and skipped (tested with a real
  mainnet NFT transfer).
- Addresses are printed lowercase: EIP-55 checksums need Keccak-256. Input in any
  case is accepted; the checksum is not verified.
- Adding events with dynamic types (strings, arrays) would justify revisiting
  this; `Permit2` and NFT approvals (roadmap) are still fixed-layout.
