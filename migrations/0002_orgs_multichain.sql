-- Organizations, Sign-In with Ethereum sessions and multiple chains.
-- See docs/adr/0005-multi-tenancy.md, 0006-sign-in-with-ethereum.md and
-- 0007-multi-chain.md.
--
-- Breaking: 0001's watch list had no owner and its derived state no chain id,
-- and neither can be inferred. Derived state (events, allowances, alerts,
-- checkpoint) is dropped and re-indexed from each chain's start block; the old
-- unscoped watch list is dropped and wallets are added again per organization.

DROP TABLE alerts;
DROP TABLE allowances;
DROP TABLE events;
DROP TABLE checkpoint;
DROP TABLE wallets;

-- Accounts are Ethereum addresses: whoever signs a SIWE message for an address
-- is that account. No passwords, no e-mail.
CREATE TABLE organizations (
    id         BIGSERIAL   PRIMARY KEY,
    name       TEXT        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    org_id     BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    account    BYTEA       NOT NULL CHECK (octet_length(account) = 20),
    role       TEXT        NOT NULL CHECK (role IN ('owner', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, account)
);
CREATE INDEX memberships_account_idx ON memberships (account);

-- Single-use SIWE nonces. used_at is set once, atomically, at sign-in.
CREATE TABLE siwe_nonces (
    nonce      TEXT        PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX siwe_nonces_expires_idx ON siwe_nonces (expires_at);

-- Sessions store only the SHA-256 of the cookie token.
CREATE TABLE sessions (
    token_hash BYTEA       PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    account    BYTEA       NOT NULL CHECK (octet_length(account) = 20),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- An organization's watch list: (chain, address) pairs.
CREATE TABLE watched_wallets (
    org_id     BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    chain_id   BIGINT      NOT NULL CHECK (chain_id > 0),
    address    BYTEA       NOT NULL CHECK (octet_length(address) = 20),
    label      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, chain_id, address)
);
CREATE INDEX watched_wallets_chain_idx ON watched_wallets (chain_id, address);

-- Chain data below is shared by every organization watching the address; access
-- to it always goes through watched_wallets.
CREATE TABLE events (
    chain_id     BIGINT        NOT NULL,
    tx_hash      BYTEA         NOT NULL CHECK (octet_length(tx_hash) = 32),
    log_index    INTEGER       NOT NULL,
    block_number BIGINT        NOT NULL,
    block_hash   BYTEA         NOT NULL,
    kind         TEXT          NOT NULL CHECK (kind IN ('approval', 'transfer')),
    token        BYTEA         NOT NULL,
    from_addr    BYTEA         NOT NULL, -- owner for approvals
    to_addr      BYTEA         NOT NULL, -- spender for approvals
    amount       NUMERIC(78,0) NOT NULL CHECK (amount >= 0),
    PRIMARY KEY (chain_id, tx_hash, log_index)
);
CREATE INDEX events_block_idx ON events (chain_id, block_number);
CREATE INDEX events_approvals_idx ON events (chain_id, from_addr, token, block_number) WHERE kind = 'approval';

CREATE TABLE allowances (
    chain_id          BIGINT        NOT NULL,
    owner             BYTEA         NOT NULL,
    token             BYTEA         NOT NULL,
    spender           BYTEA         NOT NULL,
    amount            NUMERIC(78,0) NOT NULL CHECK (amount >= 0),
    updated_block     BIGINT        NOT NULL,
    updated_log_index INTEGER       NOT NULL,
    PRIMARY KEY (chain_id, owner, token, spender)
);
CREATE INDEX allowances_updated_block_idx ON allowances (chain_id, updated_block);

-- One alert per organization watching the wallet. Alerts outlive a removed
-- wallet (history) but not their organization.
CREATE TABLE alerts (
    id              BIGSERIAL   PRIMARY KEY,
    org_id          BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    chain_id        BIGINT      NOT NULL,
    wallet          BYTEA       NOT NULL,
    rule            TEXT        NOT NULL,
    severity        TEXT        NOT NULL,
    reason          TEXT        NOT NULL,
    token           BYTEA       NOT NULL,
    tx_hash         BYTEA       NOT NULL,
    log_index       INTEGER     NOT NULL,
    block_number    BIGINT      NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered')),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    UNIQUE (org_id, chain_id, rule, tx_hash, log_index)
);
CREATE INDEX alerts_due_idx ON alerts (next_attempt_at) WHERE status = 'pending';
CREATE INDEX alerts_org_created_idx ON alerts (org_id, created_at DESC);
CREATE INDEX alerts_chain_block_idx ON alerts (chain_id, block_number);

-- One row per chain: the last fully processed block and its hash.
CREATE TABLE checkpoints (
    chain_id     BIGINT      PRIMARY KEY,
    block_number BIGINT      NOT NULL,
    block_hash   BYTEA       NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
