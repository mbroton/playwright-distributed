-- name: InsertSession :one
INSERT INTO sessions (
    id,
    worker_id,
    mode,
    status,
    created_by_key,
    expires_at,
    last_heartbeat,
    keep_alive_ms,
    connect_metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: GetSession :one
SELECT *
FROM sessions
WHERE id = $1;

-- name: SetSessionStatus :one
UPDATE sessions
SET status = $2
WHERE id = $1
RETURNING *;

-- name: RenewSessionHeartbeat :one
UPDATE sessions
SET last_heartbeat = $2
WHERE id = $1
RETURNING *;

-- name: CountRunningSessionsByWorker :one
SELECT count(*)
FROM sessions
WHERE worker_id = $1
  AND status = 'running';

-- name: ListSessionsByWorker :many
SELECT *
FROM sessions
WHERE worker_id = $1
ORDER BY created_at, id;
