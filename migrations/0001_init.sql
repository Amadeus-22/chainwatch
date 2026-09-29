-- chainwatch initial schema.
-- Addresses and hashes are raw bytes (20 and 32); token amounts are uint256 values
-- stored exactly as NUMERIC(78,0) (2^256 has 78 decimal digits).

CREATE TABLE wallets (
    address    BYTEA       PRIMARY KEY CHECK (octet_length(address) = 20),
    label      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every decoded ERC-20 Approval/Transfer that involves a watched wallet.
-- (tx_hash, log_index) identifies a log on the canonical chain, which makes
-- re-indexing a range idempotent.
CREATE TABLE events (
    tx_hash      BYTEA         NOT NULL CHECK (octet_length(tx_hash) = 32),
    log_index    INTEGER       NOT NULL,
    block_number BIGINT        NOT NULL,
    block_hash   BYTEA         NOT NULL,
    kind         TEXT          NOT NULL CHECK (kind IN ('approval', 'transfer')),
    token        BYTEA         NOT NULL,
    from_addr    BYTEA         NOT NULL, -- owner for approvals
    to_addr      BYTEA         NOT NULL, -- spender for approvals
    amount       NUMERIC(78,0) NOT NULL CHECK (amount >= 0),
    PRIMARY KEY (tx_hash, log_index)
);
CREATE INDEX events_block_idx ON events (block_number);
CREATE INDEX events_approvals_idx ON events (from_addr, token, block_number) WHERE kind = 'approval';

-- Derived state: the last approved amount per (owner, token, spender).
-- updated_block/updated_log_index let an out-of-order write never overwrite a newer one.
CREATE TABLE allowances (
    owner             BYTEA         NOT NULL,
    token             BYTEA         NOT NULL,
    spender           BYTEA         NOT NULL,
    amount            NUMERIC(78,0) NOT NULL CHECK (amount >= 0),
    updated_block     BIGINT        NOT NULL,
    updated_log_index INTEGER       NOT NULL,
    PRIMARY KEY (owner, token, spender)
);
CREATE INDEX allowances_updated_block_idx ON allowances (updated_block);

CREATE TABLE alerts (
    id              BIGSERIAL   PRIMARY KEY,
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
    UNIQUE (rule, tx_hash, log_index)
);
CREATE INDEX alerts_due_idx ON alerts (next_attempt_at) WHERE status = 'pending';
CREATE INDEX alerts_wallet_created_idx ON alerts (wallet, created_at DESC);

-- Single row: the last fully processed block and its hash (for reorg detection).
CREATE TABLE checkpoint (
    id           BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (id),
    block_number BIGINT      NOT NULL,
    block_hash   BYTEA       NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
