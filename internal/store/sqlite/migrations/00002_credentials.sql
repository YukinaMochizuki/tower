-- +goose Up
-- Credentials Hangar issued, by the SHA-256 of the secret: the secret itself
-- is never stored. They live outside objects and events, so neither a watch
-- nor the revision sees them. Times are Unix milliseconds; 0 means unset.
CREATE TABLE credentials (
    hash         TEXT    PRIMARY KEY,
    type         TEXT    NOT NULL,
    family       TEXT    NOT NULL,
    principal    TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    idle_until   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    used_at      INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX credentials_family ON credentials (family);
CREATE INDEX credentials_principal ON credentials (principal);
CREATE INDEX credentials_expires_at ON credentials (expires_at);

-- Families revoked, each until its last credential would have expired: no
-- credential may be issued into one, so a refresh racing the revocation of
-- its own login cannot bring it back.
CREATE TABLE revoked_families (
    family     TEXT    PRIMARY KEY,
    expires_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX revoked_families_expires_at ON revoked_families (expires_at);

-- Logins in progress, each under an opaque key, until they finish or
-- expire. data is the caller's, opaque to the store.
CREATE TABLE auth_requests (
    key        TEXT    PRIMARY KEY,
    data       BLOB    NOT NULL,
    expires_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX auth_requests_expires_at ON auth_requests (expires_at);

-- +goose Down
DROP TABLE auth_requests;
DROP TABLE revoked_families;
DROP TABLE credentials;
