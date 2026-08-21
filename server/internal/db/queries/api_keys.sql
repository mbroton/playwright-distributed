-- name: InsertAPIKey :one
INSERT INTO api_keys (
    id,
    name,
    hash,
    prefix
) VALUES (
    $1, $2, $3, $4
)
RETURNING *;

-- name: GetActiveAPIKeyByHash :one
SELECT *
FROM api_keys
WHERE hash = $1
  AND revoked_at IS NULL;

-- name: GetAPIKey :one
SELECT *
FROM api_keys
WHERE id = $1;

-- name: CountActiveAPIKeys :one
SELECT count(*)
FROM api_keys
WHERE revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys
SET last_used_at = now()
WHERE id = $1
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokeAPIKey :one
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1
  AND revoked_at IS NULL
RETURNING *;

-- name: DeleteAPIKey :exec
DELETE FROM api_keys
WHERE id = $1;

-- name: ListAPIKeys :many
SELECT id, name, prefix, created_at, last_used_at, revoked_at
FROM api_keys
ORDER BY created_at, id;
