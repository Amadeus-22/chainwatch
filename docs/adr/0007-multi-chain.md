# 0007 — Multi-chain: one indexer per chain in one process

- Status: accepted
- Date: 2026-09-29

## Context

Treasuries hold assets on Ethereum and on L2s and sidechains (Base, Arbitrum,
Polygon). Running one chainwatch per chain means one database per chain and no
single view of an organization's exposure. The MVP listed multi-chain as a
non-goal.

## Decision

- **Configuration**: `CHAINWATCH_CHAINS_FILE` points to a YAML list of networks
  (`id`, `name`, `rpc_url`, `explorer_url`, `confirmations`, `reorg_window`,
  `max_block_range`, `start_block`, `poll_interval`, `large_outflow`), decoded
  strictly; `${VAR}` in `rpc_url` is expanded so provider keys stay in the
  environment. Environment variables remain the defaults for any field a chain
  omits. A single chain can still be configured with `CHAINWATCH_RPC_URL` and
  `CHAINWATCH_CHAIN_ID` (demo, compose). `deploy/chains.example.yaml` documents
  Ethereum, Base, Arbitrum One and Polygon with chain-appropriate confirmation
  depths.
- **Runtime**: `main` starts one `app.Indexer` per chain, each with its own JSON-RPC
  client, in the same errgroup; they share the Postgres pool, the alerter and the
  API. Chains fail independently: an RPC outage on one chain backs off only that
  indexer.
- **Data**: everything chain-derived is keyed by `chain_id` (events, allowances,
  per-chain checkpoints, alerts). A wallet is `(chain, address)`.
- **Safety**: `/readyz` has one check per chain that calls `eth_chainId` and fails
  if the node serves a different chain than configured.
- **Metrics** carry a `chain_id` label.

## Alternatives considered

- **One process per chain** sharing a database: more moving parts for operators and
  coordination for migrations, for no isolation gain at this load (polling is
  I/O-bound and cheap).
- **Chains in environment variables** (`CHAINWATCH_CHAIN_1_RPC_URL`…): unreadable
  and unvalidatable beyond two chains; ENGINEERING.md prescribes YAML for lists.

## Consequences

- Reorg handling, range halving and idempotency are unchanged, per chain.
- Tests run two indexers on fake chains against one store (same address and same
  tx hash on both chains, concurrent `Run`), and the Postgres integration tests
  check that rewinding one chain leaves the other untouched.
- Very large deployments (hundreds of chains) would want per-chain worker pools;
  not needed now.
