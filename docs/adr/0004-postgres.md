# 0004 — PostgreSQL as the store

- Status: accepted
- Date: 2026-09-29

## Context

chainwatch stores events, derived allowances, alerts with delivery state, the
watch list and a checkpoint. It needs atomic multi-row commits (a block range and
its checkpoint together), unique constraints for idempotency, exact storage of
`uint256` amounts, and concurrent readers (API) while the indexer writes.

## Decision

Use PostgreSQL through `pgx/v5` (pool), with SQL migrations embedded from
`migrations/` and applied at startup under an advisory lock.

## Reasons

- **Atomicity**: `CommitRange` and `Rewind` are single transactions, which is what
  makes restart and reorg handling correct.
- **Idempotency in the schema**: `PRIMARY KEY (tx_hash, log_index)` on events,
  `UNIQUE (rule, tx_hash, log_index)` on alerts, `ON CONFLICT DO NOTHING`.
- **Exact amounts**: `NUMERIC(78,0)` holds any `uint256`; amounts travel as
  decimal strings, never floats.
- **Ordering guard**: allowances upsert only when `(updated_block, log_index)` is
  newer, so an out-of-order write cannot regress state.
- **Operations**: one managed service in any cloud, readable with `psql`, and the
  same engine the sibling services use.

## Alternatives considered

- **SQLite** (`modernc.org/sqlite`, allowed): simpler to run, but a single writer
  and a local file do not fit a service that should run as a container with an API
  replica. It would be a fine choice for a CLI edition.
- **A key-value store**: loses the SQL needed for the allowance rebuild on rewind
  and for API filters.

## Consequences

- Postgres is required even for local runs (`docker compose up -d postgres`).
- Adapter tests run against a real Postgres behind the `integration` build tag
  (`make test-integration`), each in a throwaway schema; CI runs them with a
  Postgres service container.
