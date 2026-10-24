-- +goose Up
CREATE TABLE objects (
    kind     TEXT    NOT NULL,
    project  TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    revision INTEGER NOT NULL,
    data     BLOB    NOT NULL,
    PRIMARY KEY (kind, project, name)
) WITHOUT ROWID;

CREATE TABLE events (
    revision INTEGER PRIMARY KEY,
    type     INTEGER NOT NULL,
    kind     TEXT    NOT NULL,
    project  TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    data     BLOB    NOT NULL
);

-- Holds 'revision' (the last revision written) and 'compacted' (events at
-- and below it are gone).
CREATE TABLE meta (
    key   TEXT    PRIMARY KEY,
    value INTEGER NOT NULL
) WITHOUT ROWID;

INSERT INTO meta (key, value) VALUES ('revision', 0), ('compacted', 0);

-- +goose Down
DROP TABLE meta;
DROP TABLE events;
DROP TABLE objects;
