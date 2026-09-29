# 0002 — Reorg handling: checkpoint hash, rewind a window, re-index

- Status: accepted
- Date: 2026-09-29

## Context

A chain reorganisation replaces recent blocks. Anything chainwatch derived from a
replaced block (events, allowances, alerts) can be wrong: an approval may vanish, or
land in a different block. Indexing only confirmed blocks (`head - confirmations`)
makes this rare but not impossible, and some chains reorg deeper than others.

## Decision

1. The checkpoint stores the **number and hash** of the last committed block.
2. Before indexing, compare the stored hash with the chain's canonical hash at that
   height. On mismatch: `chainwatch_reorgs_total++`, rewind to
   `checkpoint - reorg_window`, and in **one transaction**
   - delete events above that height,
   - delete *pending* alerts above that height,
   - delete allowances last set above that height and rebuild them from the latest
     surviving approval,
   - set the checkpoint to the rewind block and its current canonical hash.
3. Re-index from there. Inserts are idempotent (events unique on
   `(tx_hash, log_index)`, alerts on `(rule, tx_hash, log_index)`), so blocks that
   did not actually change produce no duplicates.
4. For every range, read the hash of the range's last block **before** reading
   its logs. If a reorg lands between the two calls, the stored hash is the stale
   one and the next sync detects the mismatch. The opposite order could pair stale
   logs with a fresh hash and hide the reorg.

## Alternatives considered

- **Store the hash of every processed block and walk back to the fork point.**
  Precise, but it needs a block-hash table and one RPC per walked block. With
  confirmations in front, a fixed window is simpler and costs one re-index of
  `reorg_window` blocks on a rare event.
- **Trust `removed: true` in logs.** Only subscriptions deliver it reliably.

## Consequences

- Detection covers reorgs deeper than `confirmations` and up to
  `confirmations + reorg_window` blocks (12 + 64 by default). A deeper reorg is
  detected (hash mismatch) but only the last `reorg_window` blocks are re-indexed.
- **Delivered alerts are kept.** A notification already sent cannot be unsent; if
  its transaction was reorged out, the alert remains in history. If the same
  transaction is re-included, the unique key prevents a second alert.
- Tested with a fake chain that swaps a fork in (`TestSyncRewindsOnReorg`) and
  against Postgres (`TestRewindRebuildsAllowancesAndDropsPendingAlerts`).
