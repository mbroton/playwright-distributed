-- name: RegisterWorker :one
INSERT INTO workers (
    id,
    address,
    browser,
    playwright_version,
    max_slots,
    status,
    last_heartbeat
) VALUES (
    $1, $2, $3, $4, $5, $6, now()
)
RETURNING *;

-- name: GetWorker :one
SELECT *
FROM workers
WHERE id = $1;

-- name: UpdateWorkerHeartbeat :one
UPDATE workers
SET last_heartbeat = now()
WHERE id = $1
RETURNING *;

-- name: SetWorkerStatus :one
UPDATE workers
SET status = $2
WHERE id = $1
RETURNING *;

-- name: ListWorkers :many
SELECT *
FROM workers
ORDER BY created_at, id;

-- name: DeleteWorker :exec
DELETE FROM workers
WHERE id = $1;
