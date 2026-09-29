# 0001 — Poll `eth_getLogs` instead of WebSocket subscriptions

- Status: accepted
- Date: 2026-09-29

## Context

chainwatch must see every ERC-20 `Approval` and `Transfer` that involves a watched
wallet, exactly once per canonical log, after `confirmations` blocks. Two ways to
get logs from a node:

1. `eth_subscribe("logs", filter)` over WebSocket: pushes logs as blocks arrive.
2. `eth_getLogs(fromBlock, toBlock, filter)` over HTTP, polled on an interval.

## Decision

Poll `eth_getLogs` over HTTP in block ranges, `confirmations` blocks behind head,
and commit a checkpoint (block number + hash) with every range.

## Reasons

- **Gaps are the failure that matters.** A subscription delivers only while the
  socket is up; after a disconnect or restart the client must backfill with
  `eth_getLogs` anyway. Polling makes the backfill path the *only* path, so it is
  the one that is tested and exercised every few seconds.
- **Confirmations are natural.** Subscriptions push unconfirmed logs and later
  `removed: true` notices; handling them means buffering by depth. A range query up
  to `head - confirmations` never sees unconfirmed data.
- **Works everywhere.** Every provider and proxy (including `rpcgate`) speaks
  HTTP JSON-RPC; WebSocket support and limits vary, and load balancers drop idle
  sockets.
- **Restart is trivial.** Resume from the checkpoint; ranges are idempotent (unique
  on tx hash + log index).
- **Latency is acceptable.** Alerts are raised after confirmations anyway
  (12 blocks ≈ 2.4 min on mainnet); a 6 s poll adds at most one block.

## Consequences

- One `eth_blockNumber` + one `eth_getBlockByNumber` + two `eth_getLogs` per range
  (owner/sender filter and recipient filter), plus cheap lookups on risky events.
- Providers cap range size or result count. The indexer halves the range on
  refusal down to a single block, then grows it back (tested in `internal/app`).
- Mempool (pre-confirmation) monitoring is out of scope; it would need
  subscriptions and is a non-goal of the MVP.
