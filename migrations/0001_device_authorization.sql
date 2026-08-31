CREATE TABLE oauth_clients (
    client_id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    allowed_scopes TEXT NOT NULL,
    default_scopes TEXT NOT NULL,
    token_ttl_seconds INTEGER NOT NULL CHECK (token_ttl_seconds > 0)
);

INSERT INTO oauth_clients (
    client_id,
    display_name,
    allowed_scopes,
    default_scopes,
    token_ttl_seconds
) VALUES (
    'ovdb-cli',
    'OpenVaultDB CLI',
    'account:read',
    'account:read',
    31536000
);

CREATE TABLE device_authorizations (
    id TEXT PRIMARY KEY,
    device_code_hash TEXT NOT NULL UNIQUE,
    user_code_hash TEXT NOT NULL UNIQUE,
    client_id TEXT NOT NULL REFERENCES oauth_clients(client_id),
    scopes TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('pending', 'approved', 'denied', 'consumed', 'expired')
    ),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    poll_interval_seconds INTEGER NOT NULL,
    last_poll_at INTEGER,
    user_id TEXT,
    user_email TEXT,
    user_name TEXT,
    decided_at INTEGER,
    consumed_at INTEGER
);

CREATE INDEX device_authorizations_expiry
    ON device_authorizations (status, expires_at);

CREATE TABLE access_tokens (
    id TEXT PRIMARY KEY,
    authorization_id TEXT NOT NULL UNIQUE
        REFERENCES device_authorizations(id),
    token_hash TEXT NOT NULL UNIQUE,
    client_id TEXT NOT NULL REFERENCES oauth_clients(client_id),
    user_id TEXT NOT NULL,
    user_email TEXT,
    user_name TEXT,
    scopes TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER,
    last_used_at INTEGER
);

CREATE INDEX access_tokens_active
    ON access_tokens (token_hash, revoked_at, expires_at);
