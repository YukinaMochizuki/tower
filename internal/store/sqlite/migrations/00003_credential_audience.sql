-- +goose Up
-- The one host a credential counts on, such as a private endpoint's
-- session cookie; empty for a credential that counts wherever its type does.
ALTER TABLE credentials ADD COLUMN audience TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE credentials DROP COLUMN audience;
