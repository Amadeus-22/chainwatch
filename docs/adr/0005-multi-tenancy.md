# 0005 — Multi-tenancy: organizations own watch lists and alerts

- Status: accepted
- Date: 2026-09-29

## Context

chainwatch is sold to treasuries, funds and DAOs. Several teams share one
deployment, and a team must never see another team's wallets, exposure or alerts:
the watch list alone reveals which addresses a fund controls. Until now the watch
list, allowances and alerts were global and the API had no authentication.

## Decision

- **Organization** is the tenant. Accounts (Ethereum addresses, see ADR 0006) are
  members with a role: `owner` (manages members) or `member` (reads data, manages
  the watch list). An organization cannot lose its last owner.
- A **watched wallet** is `(organization, chain, address)`. The same address may be
  watched by several organizations and on several chains.
- **Chain data is shared, access is not.** Events, allowances and checkpoints are
  keyed by `(chain, …)` only: a public log is stored and indexed once however many
  tenants watch the address. Every read of it joins `watched_wallets` on the
  caller's organization, so an organization sees allowances only of wallets it
  watches.
- **Alerts belong to one organization.** The indexer emits one alert per
  organization watching the wallet, unique on `(org, chain, rule, tx, log index)`.
  Each tenant has its own delivery state, and deleting an organization deletes its
  alerts.
- **Authorization lives in `app.Workspace`.** Every use case takes the signed-in
  principal and checks membership (and role) before touching the store; HTTP
  handlers only map errors to status codes. The SQL scopes by organization as well,
  as a second line of defence. Non-members get `403`.

## Alternatives considered

- **Row-level security in Postgres**: strong, but moves the rule out of reviewed Go
  code into policies and session variables, and complicates the pool. The app-layer
  check plus scoped SQL is enough at this size; revisit if raw SQL access is ever
  given to tenants.
- **Schema or database per tenant**: isolates perfectly but multiplies indexing of
  the same public logs and makes migrations N times more expensive.
- **Copying chain data per tenant**: duplicates storage and RPC work for no gain;
  the data is public.

## Consequences

- Migration `0002` drops the unscoped watch list and all derived state (they
  cannot be attributed to an organization or chain); a pre-1.0 install re-adds its
  wallets and re-indexes from `start_block`.
- Tests prove isolation twice: `internal/app/workspace_test.go` (every use case
  refuses a foreign organization) and `internal/adapter/postgres`
  (`TestTenantIsolation`, integration tag), plus HTTP tests with two signed-in users.
- Alert delivery still goes to one process-wide notifier; per-organization
  webhooks are on the roadmap.
