-- name: GetObject :one
SELECT kind, project, name, revision, data
FROM objects
WHERE kind = ? AND project = ? AND name = ?;

-- name: ListObjects :many
SELECT kind, project, name, revision, data
FROM objects
WHERE kind = sqlc.arg(kind)
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR project = sqlc.arg(project))
  AND (project > sqlc.arg(after_project)
       OR (project = sqlc.arg(after_project) AND name > sqlc.arg(after_name)))
ORDER BY project, name
LIMIT sqlc.arg(max_rows);

-- name: InsertObject :exec
INSERT INTO objects (kind, project, name, revision, data)
VALUES (?, ?, ?, ?, ?);

-- name: UpdateObject :exec
UPDATE objects
SET revision = ?, data = ?
WHERE kind = ? AND project = ? AND name = ?;

-- name: DeleteObject :exec
DELETE FROM objects
WHERE kind = ? AND project = ? AND name = ?;

-- name: InsertEvent :exec
INSERT INTO events (revision, type, kind, project, name, data)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListEvents :many
SELECT revision, type, kind, project, name, data
FROM events
WHERE revision > sqlc.arg(after)
  AND revision <= sqlc.arg(through)
  AND (CAST(sqlc.arg(kind) AS TEXT) = '' OR kind = sqlc.arg(kind))
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR project = sqlc.arg(project))
ORDER BY revision
LIMIT sqlc.arg(max_rows);

-- name: DeleteEventsThrough :exec
DELETE FROM events
WHERE revision <= ?;

-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
UPDATE meta SET value = ? WHERE key = ?;

-- name: InsertCredential :exec
INSERT INTO credentials (hash, type, family, principal, created_at, expires_at, idle_until, last_used_at, used_at, audience)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetCredential :one
SELECT hash, type, family, principal, created_at, expires_at, idle_until, last_used_at, used_at, audience
FROM credentials
WHERE hash = ?;

-- name: TouchCredential :execrows
UPDATE credentials
SET last_used_at = ?, idle_until = ?
WHERE hash = ?;

-- name: ConsumeCredential :execrows
UPDATE credentials
SET used_at = sqlc.arg(used_at)
WHERE hash = sqlc.arg(hash) AND used_at = 0;

-- name: DeleteCredentialFamily :execrows
DELETE FROM credentials
WHERE family = ?;

-- name: FamilyHolds :one
SELECT COUNT(*)
FROM credentials
WHERE family = sqlc.arg(family) AND type = sqlc.arg(type)
  AND expires_at > sqlc.arg(now)
  AND (idle_until = 0 OR idle_until > sqlc.arg(now));

-- name: FamilyExpiry :one
SELECT CAST(COALESCE(MAX(expires_at), 0) AS INTEGER) AS expires_at
FROM credentials
WHERE family = ?;

-- name: FamiliesOf :many
SELECT family, CAST(MAX(expires_at) AS INTEGER) AS expires_at
FROM credentials
WHERE principal = ?
GROUP BY family;

-- name: RevokeFamily :exec
INSERT INTO revoked_families (family, expires_at)
VALUES (?, ?)
ON CONFLICT (family) DO UPDATE SET expires_at = MAX(revoked_families.expires_at, excluded.expires_at);

-- name: FamilyRevoked :one
SELECT COUNT(*) FROM revoked_families
WHERE family = ?;

-- name: DeleteExpiredRevokedFamilies :exec
DELETE FROM revoked_families
WHERE expires_at <= ?;

-- name: CountAuthRequests :one
SELECT COUNT(*) FROM auth_requests
WHERE substr(key, 1, length(CAST(sqlc.arg(prefix) AS TEXT))) = CAST(sqlc.arg(prefix) AS TEXT)
  AND expires_at > sqlc.arg(now);

-- name: DeleteExpiredCredentials :exec
DELETE FROM credentials
WHERE expires_at <= sqlc.arg(now)
   OR (idle_until <> 0 AND idle_until <= sqlc.arg(now));

-- name: InsertAuthRequest :exec
INSERT INTO auth_requests (key, data, expires_at)
VALUES (?, ?, ?);

-- name: GetAuthRequest :one
SELECT key, data, expires_at
FROM auth_requests
WHERE key = ?;

-- name: SwapAuthRequest :execrows
UPDATE auth_requests
SET data = sqlc.arg(data)
WHERE key = sqlc.arg(key) AND data = sqlc.arg(old);

-- name: DeleteAuthRequest :execrows
DELETE FROM auth_requests
WHERE key = ?;

-- name: DeleteExpiredAuthRequests :exec
DELETE FROM auth_requests
WHERE expires_at <= ?;
